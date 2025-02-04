package repos

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/grafana/regexp"
	regexpsyntax "github.com/grafana/regexp/syntax"
	"golang.org/x/sync/errgroup"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/envvar"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/authz"
	livedependencies "github.com/sourcegraph/sourcegraph/internal/codeintel/dependencies/live"
	codeintelTypes "github.com/sourcegraph/sourcegraph/internal/codeintel/types"
	"github.com/sourcegraph/sourcegraph/internal/conf"
	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/gitserver"
	"github.com/sourcegraph/sourcegraph/internal/gitserver/gitdomain"
	"github.com/sourcegraph/sourcegraph/internal/search"
	"github.com/sourcegraph/sourcegraph/internal/search/limits"
	"github.com/sourcegraph/sourcegraph/internal/search/query"
	"github.com/sourcegraph/sourcegraph/internal/search/searchcontexts"
	"github.com/sourcegraph/sourcegraph/internal/trace"
	"github.com/sourcegraph/sourcegraph/internal/types"
	"github.com/sourcegraph/sourcegraph/lib/errors"
	"github.com/sourcegraph/sourcegraph/lib/group"
)

type Resolved struct {
	RepoRevs []*search.RepositoryRevisions

	MissingRepoRevs []RepoRevSpecs

	// Next points to the next page of resolved repository revisions. It will
	// be nil if there are no more pages left.
	Next types.MultiCursor
}

func (r *Resolved) String() string {
	return fmt.Sprintf("Resolved{RepoRevs=%d, MissingRepoRevs=%d}", len(r.RepoRevs), len(r.MissingRepoRevs))
}

func NewResolver(db database.DB) *Resolver {
	return &Resolver{
		db:        db,
		gitserver: gitserver.NewClient(db),
	}
}

type Resolver struct {
	db        database.DB
	gitserver gitserver.Client
}

func (r *Resolver) Paginate(ctx context.Context, opts search.RepoOptions, handle func(*Resolved) error) (err error) {
	tr, ctx := trace.New(ctx, "searchrepos.Paginate", "")
	defer func() {
		tr.SetError(err)
		tr.Finish()
	}()

	if opts.Limit == 0 {
		opts.Limit = 500
	}

	var errs error

	for {
		page, err := r.Resolve(ctx, opts)
		if err != nil {
			errs = errors.Append(errs, err)
			if !errors.Is(err, &MissingRepoRevsError{}) { // Non-fatal errors
				break
			}
		}
		tr.LazyPrintf("resolved %d repos, %d missing", len(page.RepoRevs), len(page.MissingRepoRevs))

		if err = handle(&page); err != nil {
			errs = errors.Append(errs, err)
			break
		}

		if page.Next == nil {
			break
		}

		opts.Cursors = page.Next
	}

	return errs
}

func (r *Resolver) Resolve(ctx context.Context, op search.RepoOptions) (_ Resolved, errs error) {
	tr, ctx := trace.New(ctx, "searchrepos.Resolve", op.String())
	defer func() {
		tr.SetError(errs)
		tr.Finish()
	}()

	excludePatterns := op.MinusRepoFilters
	includePatterns, includePatternRevs, errs := findPatternRevs(op.RepoFilters)
	if errs != nil {
		return Resolved{}, errs
	}

	limit := op.Limit
	if limit == 0 {
		limit = limits.SearchLimits(conf.Get()).MaxRepos
	}

	dependencyNames, dependencyRevs, dependencyNotFoundRevs, errs := r.fetchDependencyInfo(ctx, &op)
	if errs != nil {
		return Resolved{}, errs
	}

	searchContext, errs := searchcontexts.ResolveSearchContextSpec(ctx, r.db, op.SearchContextSpec)
	if errs != nil {
		return Resolved{}, errs
	}

	options := database.ReposListOptions{
		IncludePatterns:       includePatterns,
		Names:                 dependencyNames,
		ExcludePattern:        query.UnionRegExps(excludePatterns),
		DescriptionPatterns:   op.DescriptionPatterns,
		CaseSensitivePatterns: op.CaseSensitiveRepoFilters,
		Cursors:               op.Cursors,
		// List N+1 repos so we can see if there are repos omitted due to our repo limit.
		LimitOffset:  &database.LimitOffset{Limit: limit + 1},
		NoForks:      op.NoForks,
		OnlyForks:    op.OnlyForks,
		NoArchived:   op.NoArchived,
		OnlyArchived: op.OnlyArchived,
		NoPrivate:    op.Visibility == query.Public,
		OnlyPrivate:  op.Visibility == query.Private,
		OnlyCloned:   op.OnlyCloned,
		OrderBy: database.RepoListOrderBy{
			{
				Field:      database.RepoListStars,
				Descending: true,
				Nulls:      "LAST",
			},
			{
				Field:      database.RepoListID,
				Descending: true,
			},
		},
	}

	// Filter by search context repository revisions only if this search context doesn't have
	// a query, which replaces the context:foo term at query parsing time.
	if searchContext.Query == "" {
		options.SearchContextID = searchContext.ID
		options.UserID = searchContext.NamespaceUserID
		options.OrgID = searchContext.NamespaceOrgID
		options.IncludeUserPublicRepos = searchContext.ID == 0 && searchContext.NamespaceUserID != 0
	}

	tr.LazyPrintf("Repos.ListMinimalRepos - start")
	repos, errs := r.db.Repos().ListMinimalRepos(ctx, options)
	tr.LazyPrintf("Repos.ListMinimalRepos - done (%d repos, err %v)", len(repos), errs)

	if errs != nil {
		return Resolved{}, errs
	}

	if len(repos) == 0 && len(op.Cursors) == 0 { // Is the first page empty?
		return Resolved{}, ErrNoResolvedRepos
	}

	var next types.MultiCursor
	if len(repos) == limit+1 { // Do we have a next page?
		last := repos[len(repos)-1]
		for _, o := range options.OrderBy {
			c := types.Cursor{Column: string(o.Field)}

			switch c.Column {
			case "stars":
				c.Value = strconv.FormatInt(int64(last.Stars), 10)
			case "id":
				c.Value = strconv.FormatInt(int64(last.ID), 10)
			}

			if o.Descending {
				c.Direction = "prev"
			} else {
				c.Direction = "next"
			}

			next = append(next, &c)
		}
		repos = repos[:len(repos)-1]
	}

	var searchContextRepositoryRevisions map[api.RepoID]RepoRevSpecs
	if !searchcontexts.IsAutoDefinedSearchContext(searchContext) && searchContext.Query == "" {
		scRepoRevs, err := searchcontexts.GetRepositoryRevisions(ctx, r.db, searchContext.ID)
		if err != nil {
			return Resolved{}, err
		}

		searchContextRepositoryRevisions = make(map[api.RepoID]RepoRevSpecs, len(scRepoRevs))
		for _, repoRev := range scRepoRevs {
			revSpecs := make([]search.RevisionSpecifier, 0, len(repoRev.Revs))
			for _, rev := range repoRev.Revs {
				revSpecs = append(revSpecs, search.RevisionSpecifier{RevSpec: rev})
			}
			searchContextRepositoryRevisions[repoRev.Repo.ID] = RepoRevSpecs{
				Repo: repoRev.Repo,
				Revs: revSpecs,
			}
		}
	}

	tr.LazyPrintf("starting rev association")
	associatedRepoRevs, missingRepoRevs := r.associateReposWithRevs(repos, dependencyRevs, searchContextRepositoryRevisions, includePatternRevs)
	tr.LazyPrintf("completed rev association")

	tr.LazyPrintf("starting glob expansion")
	normalized, normalizedMissingRepoRevs, err := r.normalizeRefs(ctx, associatedRepoRevs)
	missingRepoRevs = append(missingRepoRevs, normalizedMissingRepoRevs...)
	if err != nil {
		return Resolved{}, err
	}
	tr.LazyPrintf("finished glob expansion")

	tr.LazyPrintf("starting rev filtering")
	filteredRepoRevs, err := r.filterHasCommitAfter(ctx, normalized, op)
	tr.LazyPrintf("completed rev filtering")

	if len(missingRepoRevs) > 0 {
		err = errors.Append(err, &MissingRepoRevsError{Missing: missingRepoRevs})
	}

	if len(dependencyNotFoundRevs) > 0 {
		for repo, revs := range dependencyNotFoundRevs {
			err = errors.Append(err, &MissingLockfileIndexing{repo: repo, revs: revs})
		}
	}

	return Resolved{
		RepoRevs:        filteredRepoRevs,
		MissingRepoRevs: missingRepoRevs,
		Next:            next,
	}, err
}

// associateReposWithRevs re-associates revisions with the repositories fetched from the db
func (r *Resolver) associateReposWithRevs(
	repos []types.MinimalRepo,
	dependencyRevs map[api.RepoName][]search.RevisionSpecifier,
	searchContextRepoRevs map[api.RepoID]RepoRevSpecs,
	includePatternRevs []patternRevspec,
) (
	associated []RepoRevSpecs,
	missing []RepoRevSpecs,
) {
	g := group.New().WithMaxConcurrency(8)

	associatedRevs := make([]RepoRevSpecs, len(repos))
	revsAreMissing := make([]bool, len(repos))

	for i, repo := range repos {
		i, repo := i, repo
		g.Go(func() {
			var (
				revs      []search.RevisionSpecifier
				isMissing bool
			)

			if len(dependencyRevs) > 0 {
				revs = dependencyRevs[repo.Name]
			}

			if len(searchContextRepoRevs) > 0 && len(revs) == 0 {
				if scRepoRev, ok := searchContextRepoRevs[repo.ID]; ok {
					revs = scRepoRev.Revs
				}
			}

			if len(revs) == 0 {
				var clashingRevs []search.RevisionSpecifier
				revs, clashingRevs = getRevsForMatchedRepo(repo.Name, includePatternRevs)

				// if multiple specified revisions clash, report this usefully:
				if len(revs) == 0 && len(clashingRevs) != 0 {
					revs = clashingRevs
					isMissing = true
				}
			}

			associatedRevs[i] = RepoRevSpecs{Repo: repo, Revs: revs}
			revsAreMissing[i] = isMissing
		})
	}

	g.Wait()

	// Sort missing revs to the end, but maintain order otherwise.
	sort.SliceStable(associatedRevs, func(i, j int) bool {
		return !revsAreMissing[i] && revsAreMissing[j]
	})

	notMissingCount := 0
	for _, isMissing := range revsAreMissing {
		if !isMissing {
			notMissingCount++
		}
	}

	return associatedRevs[:notMissingCount], associatedRevs[notMissingCount:]
}

// normalizeRefs handles three jobs:
// 1) expanding each ref glob into a set of refs
// 2) checking that every revision (except HEAD) exists
// 3) expanding the empty string revision (which implicitly means HEAD) into an explicit "HEAD"
func (r *Resolver) normalizeRefs(ctx context.Context, repoRevSpecs []RepoRevSpecs) ([]*search.RepositoryRevisions, []RepoRevSpecs, error) {
	results := make([]*search.RepositoryRevisions, len(repoRevSpecs))

	var (
		mu         sync.Mutex
		missing    []RepoRevSpecs
		addMissing = func(revSpecs RepoRevSpecs) {
			mu.Lock()
			missing = append(missing, revSpecs)
			mu.Unlock()
		}
	)

	g := group.New().WithContext(ctx).WithMaxConcurrency(128)
	for i, repoRev := range repoRevSpecs {
		i, repoRev := i, repoRev
		g.Go(func(ctx context.Context) error {
			expanded, err := r.normalizeRepoRefs(ctx, repoRev.Repo, repoRev.Revs, addMissing)
			if err != nil {
				return err
			}
			results[i] = &search.RepositoryRevisions{
				Repo: repoRev.Repo,
				Revs: expanded,
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, err
	}

	// Filter out any results whose revSpecs expanded to nothing
	filteredResults := results[:0]
	for _, result := range results {
		if len(result.Revs) > 0 {
			filteredResults = append(filteredResults, result)
		}
	}

	return filteredResults, missing, nil
}

func (r *Resolver) normalizeRepoRefs(
	ctx context.Context,
	repo types.MinimalRepo,
	revSpecs []search.RevisionSpecifier,
	reportMissing func(RepoRevSpecs),
) ([]string, error) {
	revs := make([]string, 0, len(revSpecs))
	var globs []gitdomain.RefGlob
	for _, rev := range revSpecs {
		switch {
		case rev.RefGlob != "":
			globs = append(globs, gitdomain.RefGlob{Include: rev.RefGlob})
		case rev.ExcludeRefGlob != "":
			globs = append(globs, gitdomain.RefGlob{Exclude: rev.ExcludeRefGlob})
		case rev.RevSpec == "" || rev.RevSpec == "HEAD":
			// NOTE: HEAD is the only case here that we don't resolve to a
			// commit ID. We should consider building []gitdomain.Ref here
			// instead of just []string because we have the exact commit hashes,
			// so we could avoid resolving later.
			revs = append(revs, rev.RevSpec)
		case rev.RevSpec != "":
			trimmedRev := strings.TrimPrefix(rev.RevSpec, "^")
			_, err := r.gitserver.ResolveRevision(ctx, repo.Name, trimmedRev, gitserver.ResolveRevisionOptions{NoEnsureRevision: true})
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.HasType(err, gitdomain.BadCommitError{}) {
					return nil, err
				}
				reportMissing(RepoRevSpecs{Repo: repo, Revs: []search.RevisionSpecifier{rev}})
				continue
			}
			revs = append(revs, rev.RevSpec)
		}
	}

	if len(globs) == 0 {
		// Happy path with no globs to expand
		return revs, nil
	}

	rg, err := gitdomain.CompileRefGlobs(globs)
	if err != nil {
		return nil, err
	}

	allRefs, err := r.gitserver.ListRefs(ctx, repo.Name)
	if err != nil {
		return nil, err
	}

	for _, ref := range allRefs {
		if rg.Match(ref.Name) {
			revs = append(revs, strings.TrimPrefix(ref.Name, "refs/heads/"))
		}
	}

	return revs, nil

}

// filterHasCommitAfter filters the revisions on each of a set of RepositoryRevisions to ensure that
// any repo-level filters (e.g. `repo:contains.commit.after()`) apply to this repo/rev combo.
func (r *Resolver) filterHasCommitAfter(
	ctx context.Context,
	repoRevs []*search.RepositoryRevisions,
	op search.RepoOptions,
) (
	[]*search.RepositoryRevisions,
	error,
) {
	// Early return if HasCommitAfter is not set
	if op.CommitAfter == "" {
		return repoRevs, nil
	}

	g := group.New().WithContext(ctx).WithMaxConcurrency(128)

	for _, repoRev := range repoRevs {
		repoRev := repoRev

		allRevs := repoRev.Revs

		var mu sync.Mutex
		repoRev.Revs = make([]string, 0, len(allRevs))

		for _, rev := range allRevs {
			rev := rev
			g.Go(func(ctx context.Context) error {
				if hasCommitAfter, err := r.gitserver.HasCommitAfter(ctx, repoRev.Repo.Name, op.CommitAfter, rev, authz.DefaultSubRepoPermsChecker); err != nil {
					if !errors.HasType(err, &gitdomain.RevisionNotFoundError{}) && !gitdomain.IsRepoNotExist(err) {
						return err
					}
					return err
				} else if !hasCommitAfter {
					return nil
				}

				mu.Lock()
				repoRev.Revs = append(repoRev.Revs, rev)
				mu.Unlock()
				return nil
			})
		}
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Filter out any repo revs with empty revs
	filteredRepoRevs := repoRevs[:0]
	for _, repoRev := range repoRevs {
		if len(repoRev.Revs) > 0 {
			filteredRepoRevs = append(filteredRepoRevs, repoRev)
		}
	}

	return filteredRepoRevs, nil
}

// computeExcludedRepos computes the ExcludedRepos that the given RepoOptions would not match. This is
// used to show in the search UI what repos are excluded precisely.
func computeExcludedRepos(ctx context.Context, db database.DB, op search.RepoOptions) (ex ExcludedRepos, err error) {
	tr, ctx := trace.New(ctx, "searchrepos.Excluded", op.String())
	defer func() {
		tr.LazyPrintf("excluded repos: %+v", ex)
		tr.SetError(err)
		tr.Finish()
	}()

	excludePatterns := op.MinusRepoFilters
	includePatterns, _, err := findPatternRevs(op.RepoFilters)
	if err != nil {
		return ExcludedRepos{}, err
	}

	limit := op.Limit
	if limit == 0 {
		limit = limits.SearchLimits(conf.Get()).MaxRepos
	}

	if len(op.Dependencies) > 0 {
		// Dependency search only operates on package repos. Since package repos
		// cannot be archives or forks, there will never be any excluded repos for
		// dependency search, so we can avoid doing extra work here.
		return ExcludedRepos{}, nil
	}

	searchContext, err := searchcontexts.ResolveSearchContextSpec(ctx, db, op.SearchContextSpec)
	if err != nil {
		return ExcludedRepos{}, err
	}

	options := database.ReposListOptions{
		IncludePatterns: includePatterns,
		ExcludePattern:  query.UnionRegExps(excludePatterns),
		// List N+1 repos so we can see if there are repos omitted due to our repo limit.
		LimitOffset:            &database.LimitOffset{Limit: limit + 1},
		NoForks:                op.NoForks,
		OnlyForks:              op.OnlyForks,
		NoArchived:             op.NoArchived,
		OnlyArchived:           op.OnlyArchived,
		NoPrivate:              op.Visibility == query.Public,
		OnlyPrivate:            op.Visibility == query.Private,
		SearchContextID:        searchContext.ID,
		UserID:                 searchContext.NamespaceUserID,
		OrgID:                  searchContext.NamespaceOrgID,
		IncludeUserPublicRepos: searchContext.ID == 0 && searchContext.NamespaceUserID != 0,
	}

	g, ctx := errgroup.WithContext(ctx)

	var excluded struct {
		sync.Mutex
		ExcludedRepos
	}

	if !op.ForkSet && !ExactlyOneRepo(includePatterns) {
		g.Go(func() error {
			// 'fork:...' was not specified and Forks are excluded, find out
			// which repos are excluded.
			selectForks := options
			selectForks.OnlyForks = true
			selectForks.NoForks = false
			numExcludedForks, err := db.Repos().Count(ctx, selectForks)
			if err != nil {
				return err
			}

			excluded.Lock()
			excluded.Forks = numExcludedForks
			excluded.Unlock()

			return nil
		})
	}

	if !op.ArchivedSet && !ExactlyOneRepo(includePatterns) {
		g.Go(func() error {
			// Archived...: was not specified and archives are excluded,
			// find out which repos are excluded.
			selectArchived := options
			selectArchived.OnlyArchived = true
			selectArchived.NoArchived = false
			numExcludedArchived, err := db.Repos().Count(ctx, selectArchived)
			if err != nil {
				return err
			}

			excluded.Lock()
			excluded.Archived = numExcludedArchived
			excluded.Unlock()

			return nil
		})
	}

	return excluded.ExcludedRepos, g.Wait()
}

func (r *Resolver) fetchDependencyInfo(ctx context.Context, op *search.RepoOptions) (
	dependencyNames []string,
	dependencyRevs map[api.RepoName][]search.RevisionSpecifier,
	dependencyNotFoundRevs map[api.RepoName][]search.RevisionSpecifier,
	err error,
) {
	dependencyRevs = map[api.RepoName][]search.RevisionSpecifier{}
	dependencyNotFoundRevs = map[api.RepoName][]search.RevisionSpecifier{}

	if len(op.Dependencies) > 0 {
		depNames, depRevs, notFoundRevs, err := r.dependencies(ctx, op)
		if err != nil {
			return nil, nil, nil, err
		}

		dependencyNames = append(dependencyNames, depNames...)
		for repo, revs := range depRevs {
			if _, ok := dependencyRevs[repo]; !ok {
				dependencyRevs[repo] = revs
			} else {
				dependencyRevs[repo] = append(dependencyRevs[repo], revs...)
			}
		}

		dependencyNotFoundRevs = notFoundRevs
	}

	if len(op.Dependents) > 0 {
		revDepNames, revDepRevs, err := r.dependents(ctx, op)
		if err != nil {
			return nil, nil, nil, err
		}

		dependencyNames = append(dependencyNames, revDepNames...)
		for repo, revs := range revDepRevs {
			if _, ok := dependencyRevs[repo]; !ok {
				dependencyRevs[repo] = revs
			} else {
				dependencyRevs[repo] = append(dependencyRevs[repo], revs...)
			}
		}
	}

	if (len(op.Dependencies) > 0 || len(op.Dependents) > 0) && len(dependencyNames) == 0 {
		return nil, nil, nil, ErrNoResolvedRepos
	}

	return dependencyNames, dependencyRevs, dependencyNotFoundRevs, nil
}

// dependencies resolves `repo:dependencies` predicates to a specific list of
// dependency repositories for the given repos and revision(s). It does so by:
//
// 1. Expanding each `repo:dependencies(regex@revA:revB:...)` filter regex to a list of repositories that exist in the DB.
// 2. For each of those (repo, rev) tuple, asking the code intelligence dependency API for their (transitive) dependencies.
// 3. Return those dependencies to the caller to be included in repository resolution.
func (r *Resolver) dependencies(ctx context.Context, op *search.RepoOptions) (_ []string, _ map[api.RepoName][]search.RevisionSpecifier, _ map[api.RepoName][]search.RevisionSpecifier, err error) {
	tr, ctx := trace.New(ctx, "searchrepos.dependencies", "")
	defer func() {
		tr.LazyPrintf("deps: %v", op.Dependencies)
		tr.SetError(err)
		tr.Finish()
	}()

	if !conf.DependenciesSearchEnabled() {
		return nil, nil, nil, errors.Errorf("support for `repo:dependencies()` is disabled in site config (`experimentalFeatures.dependenciesSearch`)")
	}

	repoRevs, err := listDependencyRepos(ctx, r.db.Repos(), op.Dependencies, op.CaseSensitiveRepoFilters)
	if err != nil {
		return nil, nil, nil, err
	}

	// TODO: We'll make this value depend on user input, but for now we include all dependencies.
	includeTransitive := true
	dependencyRepoRevs, notFound, err := livedependencies.GetService(r.db, livedependencies.NewSyncer()).Dependencies(ctx, repoRevs, includeTransitive)
	if err != nil {
		return nil, nil, nil, err
	}

	depRevs := make(map[api.RepoName][]search.RevisionSpecifier, len(dependencyRepoRevs))
	depNames := make([]string, 0, len(dependencyRepoRevs))

	for repoName, revs := range dependencyRepoRevs {
		depNames = append(depNames, string(repoName))
		revSpecs := make([]search.RevisionSpecifier, 0, len(revs))
		for rev := range revs {
			revSpecs = append(revSpecs, search.RevisionSpecifier{RevSpec: string(rev)})
		}
		depRevs[repoName] = revSpecs
	}

	notFoundRevs := make(map[api.RepoName][]search.RevisionSpecifier, len(notFound))
	for repoName, revs := range notFound {
		depNames = append(depNames, string(repoName))
		revSpecs := make([]search.RevisionSpecifier, 0, len(revs))
		for rev := range revs {
			revSpecs = append(revSpecs, search.RevisionSpecifier{RevSpec: string(rev)})
		}
		notFoundRevs[repoName] = revSpecs
	}

	return depNames, depRevs, notFoundRevs, nil
}

func listDependencyRepos(ctx context.Context, repoStore database.RepoStore, revSpecPatterns []string, caseSensitive bool) (map[api.RepoName]codeintelTypes.RevSpecSet, error) {
	repoRevs := make(map[api.RepoName]codeintelTypes.RevSpecSet, len(revSpecPatterns))
	for _, depParams := range revSpecPatterns {
		repoPattern, revs := search.ParseRepositoryRevisions(depParams)
		if len(revs) == 0 {
			revs = append(revs, search.RevisionSpecifier{RevSpec: "HEAD"})
		}

		rs, err := repoStore.ListMinimalRepos(ctx, database.ReposListOptions{
			IncludePatterns:       []string{repoPattern},
			CaseSensitivePatterns: caseSensitive,
		})
		if err != nil {
			return nil, err
		}

		for _, repo := range rs {
			for _, rev := range revs {
				if rev == (search.RevisionSpecifier{}) {
					rev.RevSpec = "HEAD"
				} else if rev.RevSpec == "" {
					return nil, errors.New("unsupported glob rev in dependencies filter")
				}

				if _, ok := repoRevs[repo.Name]; !ok {
					repoRevs[repo.Name] = codeintelTypes.RevSpecSet{}
				}

				repoRevs[repo.Name][api.RevSpec(rev.RevSpec)] = struct{}{}
			}
		}
	}

	return repoRevs, nil
}

func (r *Resolver) dependents(ctx context.Context, op *search.RepoOptions) (_ []string, _ map[api.RepoName][]search.RevisionSpecifier, err error) {
	tr, ctx := trace.New(ctx, "searchrepos.reverseDependencies", "")
	defer func() {
		tr.LazyPrintf("dependents: %v", op.Dependents)
		tr.SetError(err)
		tr.Finish()
	}()

	if !conf.DependenciesSearchEnabled() {
		return nil, nil, errors.Errorf("support for `repo:dependents()` is disabled in site config (`experimentalFeatures.dependenciesSearch`)")
	}

	repoRevs, err := listDependencyRepos(ctx, r.db.Repos(), op.Dependents, op.CaseSensitiveRepoFilters)
	if err != nil {
		return nil, nil, err
	}

	dependencyRepoRevs, err := livedependencies.GetService(r.db, livedependencies.NewSyncer()).Dependents(ctx, repoRevs)
	if err != nil {
		return nil, nil, err
	}

	depRevs := make(map[api.RepoName][]search.RevisionSpecifier, len(dependencyRepoRevs))
	depNames := make([]string, 0, len(dependencyRepoRevs))

	for repoName, revs := range dependencyRepoRevs {
		depNames = append(depNames, string(repoName))
		revSpecs := make([]search.RevisionSpecifier, 0, len(revs))
		for rev := range revs {
			revSpecs = append(revSpecs, search.RevisionSpecifier{RevSpec: string(rev)})
		}
		depRevs[repoName] = revSpecs
	}

	return depNames, depRevs, nil
}

// ExactlyOneRepo returns whether exactly one repo: literal field is specified and
// delineated by regex anchors ^ and $. This function helps determine whether we
// should return results for a single repo regardless of whether it is a fork or
// archive.
func ExactlyOneRepo(repoFilters []string) bool {
	if len(repoFilters) == 1 {
		filter, _ := search.ParseRepositoryRevisions(repoFilters[0])
		if strings.HasPrefix(filter, "^") && strings.HasSuffix(filter, "$") {
			filter := strings.TrimSuffix(strings.TrimPrefix(filter, "^"), "$")
			r, err := regexpsyntax.Parse(filter, regexpFlags)
			if err != nil {
				return false
			}
			return r.Op == regexpsyntax.OpLiteral
		}
	}
	return false
}

// Cf. golang/go/src/regexp/syntax/parse.go.
const regexpFlags = regexpsyntax.ClassNL | regexpsyntax.PerlX | regexpsyntax.UnicodeGroups

// ExcludedRepos is a type that counts how many repos with a certain label were
// excluded from search results.
type ExcludedRepos struct {
	Forks    int
	Archived int
}

// a patternRevspec maps an include pattern to a list of revisions
// for repos matching that pattern. "map" in this case does not mean
// an actual map, because we want regexp matches, not identity matches.
type patternRevspec struct {
	includePattern *regexp.Regexp
	revs           []search.RevisionSpecifier
}

// given a repo name, determine whether it matched any patterns for which we have
// revspecs (or ref globs), and if so, return the matching/allowed ones.
func getRevsForMatchedRepo(repo api.RepoName, pats []patternRevspec) (matched []search.RevisionSpecifier, clashing []search.RevisionSpecifier) {
	revLists := make([][]search.RevisionSpecifier, 0, len(pats))
	for _, rev := range pats {
		if rev.includePattern.MatchString(string(repo)) {
			revLists = append(revLists, rev.revs)
		}
	}
	// exactly one match: we accept that list
	if len(revLists) == 1 {
		matched = revLists[0]
		return
	}
	// no matches: we generate a dummy list containing only master
	if len(revLists) == 0 {
		matched = []search.RevisionSpecifier{{RevSpec: ""}}
		return
	}
	// if two repo specs match, and both provided non-empty rev lists,
	// we want their intersection, so we count the number of times we
	// see a revision in the rev lists, and make sure it matches the number
	// of rev lists
	revCounts := make(map[search.RevisionSpecifier]int, len(revLists[0]))

	var aliveCount int
	for i, revList := range revLists {
		aliveCount = 0
		for _, rev := range revList {
			if revCounts[rev] == i {
				aliveCount += 1
			}
			revCounts[rev] += 1
		}
	}

	if aliveCount > 0 {
		matched = make([]search.RevisionSpecifier, 0, len(revCounts))
		for rev, seenCount := range revCounts {
			if seenCount == len(revLists) {
				matched = append(matched, rev)
			}
		}
		sort.Slice(matched, func(i, j int) bool { return matched[i].Less(matched[j]) })
		return
	}

	clashing = make([]search.RevisionSpecifier, 0, len(revCounts))
	for rev := range revCounts {
		clashing = append(clashing, rev)
	}
	// ensure that lists are always returned in sorted order.
	sort.Slice(clashing, func(i, j int) bool { return clashing[i].Less(clashing[j]) })
	return
}

// findPatternRevs mutates the given list of include patterns to
// be a raw list of the repository name patterns we want, separating
// out their revision specs, if any.
func findPatternRevs(includePatterns []string) (outputPatterns []string, includePatternRevs []patternRevspec, err error) {
	outputPatterns = make([]string, 0, len(includePatterns))
	includePatternRevs = make([]patternRevspec, 0, len(includePatterns))

	for _, includePattern := range includePatterns {
		repoPattern, revs := search.ParseRepositoryRevisions(includePattern)
		// Validate pattern now so the error message is more recognizable to the
		// user
		if _, err := regexp.Compile(repoPattern); err != nil {
			return nil, nil, &badRequestError{errors.Wrap(err, "in findPatternRevs")}
		}
		repoPattern = optimizeRepoPatternWithHeuristics(repoPattern)

		outputPatterns = append(outputPatterns, repoPattern)
		if len(revs) > 0 {
			p, err := regexp.Compile("(?i:" + repoPattern + ")")
			if err != nil {
				return nil, nil, &badRequestError{err}
			}
			patternRev := patternRevspec{includePattern: p, revs: revs}
			includePatternRevs = append(includePatternRevs, patternRev)
		}
	}
	return
}

func optimizeRepoPatternWithHeuristics(repoPattern string) string {
	if envvar.SourcegraphDotComMode() && (strings.HasPrefix(repoPattern, "github.com") || strings.HasPrefix(repoPattern, `github\.com`)) {
		repoPattern = "^" + repoPattern
	}
	// Optimization: make the "." in "github.com" a literal dot
	// so that the regexp can be optimized more effectively.
	repoPattern = strings.ReplaceAll(repoPattern, "github.com", `github\.com`)
	return repoPattern
}

type badRequestError struct {
	err error
}

func (e *badRequestError) BadRequest() bool {
	return true
}

func (e *badRequestError) Error() string {
	return "bad request: " + e.err.Error()
}

func (e *badRequestError) Cause() error {
	return e.err
}

var ErrNoResolvedRepos = errors.New("no resolved repositories")

type MissingRepoRevsError struct {
	Missing []RepoRevSpecs
}

func (MissingRepoRevsError) Error() string { return "missing repo revs" }

type MissingLockfileIndexing struct {
	repo api.RepoName
	revs []search.RevisionSpecifier
}

func (e MissingLockfileIndexing) RepoName() api.RepoName { return e.repo }
func (e MissingLockfileIndexing) RevNames() (names []string) {
	for _, r := range e.revs {
		names = append(names, r.String())
	}
	return names
}

func (e MissingLockfileIndexing) Error() string {
	var out strings.Builder
	fmt.Fprintf(&out, "no index lockfiles found in %s at revision: ", e.repo)

	var revs []string
	for _, r := range e.revs {
		revs = append(revs, r.String())
	}

	fmt.Fprintf(&out, "%s", strings.Join(revs, ","))
	return out.String()
}

type RepoRevSpecs struct {
	Repo types.MinimalRepo
	Revs []search.RevisionSpecifier
}

func (r *RepoRevSpecs) RevSpecs() []string {
	res := make([]string, 0, len(r.Revs))
	for _, rev := range r.Revs {
		switch {
		case rev.RefGlob != "":
		case rev.ExcludeRefGlob != "":
		default:
			res = append(res, rev.RevSpec)
		}
	}
	return res
}
