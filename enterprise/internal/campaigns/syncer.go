package campaigns

import (
	"container/heap"
	"context"
	"sort"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sourcegraph/sourcegraph/cmd/repo-updater/repos"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/campaigns"
	"github.com/sourcegraph/sourcegraph/internal/httpcli"
	"gopkg.in/inconshreveable/log15.v2"
)

// A ChangesetSyncer periodically sync the metadata of the changesets
// saved in the database
type ChangesetSyncer struct {
	Store       *Store
	ReposStore  repos.Store
	HTTPFactory *httpcli.Factory
	// ComputeScheduleInterval determines how often a new schedule will be computed.
	// Note that it involves a DB query but no communication with codehosts
	ComputeScheduleInterval time.Duration

	priorityNotify chan []int64
	mtx            sync.Mutex
	queue          *changesetPriorityQueue
}

// Run will start the process of changeset syncing. It is long running
// and is expected to be launched once at startup.
func (s *ChangesetSyncer) Run() {
	// TODO: Setup instrumentation here
	ctx := context.Background()
	scheduleInterval := s.ComputeScheduleInterval
	if scheduleInterval == 0 {
		scheduleInterval = 2 * time.Minute
	}
	s.priorityNotify = make(chan []int64)
	s.queue = newChangesetPriorityQueue()

	// Get initial schedule
	if sched, err := s.computeSchedule(ctx); err != nil {
		// Non fatal as we'll try again later in the main loop
		log15.Error("Computing schedule", "err", err)
	} else {
		s.queue.Reschedule(sched)
	}

	// How often to refresh the schedule
	scheduleTicker := time.NewTicker(scheduleInterval)

	var (
		next      *syncSchedule
		ok        bool
		timerChan <-chan time.Time
		timer     *time.Timer
	)

	for {
		s.mtx.Lock()
		if s.queue.Len() > 0 {
			next, ok = heap.Pop(s.queue).(*syncSchedule)
		}
		s.mtx.Unlock()

		if ok {
			if timer != nil {
				timer.Stop()
			}
			if next.priority == priorityHigh {
				timer = time.NewTimer(0)
			} else {
				timer = time.NewTimer(time.Until(next.nextSync))
			}
			timerChan = timer.C
		}

		select {
		case <-scheduleTicker.C:
			sched, err := s.computeSchedule(ctx)
			if err != nil {
				log15.Error("Computing queue", "err", err)
				continue
			}
			s.mtx.Lock()
			s.queue.Reschedule(sched)
			s.mtx.Unlock()
		case <-timerChan:
			err := s.SyncChangesetByID(ctx, next.changesetID)
			if err != nil {
				log15.Error("Syncing changeset", "err", err)
			}
		case ids := <-s.priorityNotify:
			if next != nil {
				// We need to handle the currently popped item if it was in the slice
				for _, id := range ids {
					if next.changesetID == id {
						next.priority = priorityHigh
						break
					}
				}
				// We need to push the item back into the heap for the next iteration
				s.mtx.Lock()
				heap.Push(s.queue, next)
				s.mtx.Unlock()
			}
		}
	}
}

var (
	minSyncDelay = 2 * time.Minute
	maxSyncDelay = 8 * time.Hour
)

// nextSync computes the time we want the next sync to happen.
func nextSync(h campaigns.ChangesetSyncHeuristics) time.Time {
	lastSync := h.UpdatedAt
	var lastChange time.Time
	// When we perform a sync, event timestamps are all updated.
	// We should fall back to h.ExternalUpdated if the diff is small
	if diff := h.LatestEvent.Sub(lastSync); !h.LatestEvent.IsZero() && diff < minSyncDelay {
		lastChange = h.ExternalUpdatedAt
	} else {
		lastChange = maxTime(h.ExternalUpdatedAt, h.LatestEvent)
	}

	// Simple linear backoff for now
	diff := lastSync.Sub(lastChange)
	if diff > maxSyncDelay {
		diff = maxSyncDelay
	}
	if diff < minSyncDelay {
		diff = minSyncDelay
	}
	return lastSync.Add(diff)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (s *ChangesetSyncer) computeSchedule(ctx context.Context) ([]syncSchedule, error) {
	hs, err := s.Store.ListChangesetSyncHeuristics(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "listing changeset heuristics")
	}

	ss := make([]syncSchedule, len(hs))
	for i := range hs {
		nextSync := nextSync(hs[i])

		ss[i] = syncSchedule{
			changesetID: hs[i].ChangesetID,
			nextSync:    nextSync,
		}
	}

	// This will happen in the db later, for now we'll grab everything and order in code
	sort.Slice(ss, func(i, j int) bool {
		return ss[i].nextSync.Before(ss[j].nextSync)
	})

	return ss, nil
}

// EnqueueChangesetSyncs will enqueue the changesets with the supplied ids for high priority syncing.
// An error indicates that no changesets have been synced
func (s *ChangesetSyncer) EnqueueChangesetSyncs(ctx context.Context, ids []int64) error {
	if s.queue == nil {
		return errors.New("background syncing not initialised")
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	for _, id := range ids {
		item, ok := s.queue.Get(id)
		if !ok {
			continue
		}
		item.priority = priorityHigh
		s.queue.Upsert(item)
	}

	select {
	case s.priorityNotify <- ids:
	default:
	}

	return nil
}

// SyncChangesetByID will sync a single changeset given its id
func (s *ChangesetSyncer) SyncChangesetByID(ctx context.Context, id int64) error {
	log15.Debug("SyncChangesetByID", "id", id)
	cs, err := s.Store.GetChangeset(ctx, GetChangesetOpts{
		ID: id,
	})
	if err != nil {
		return err
	}
	return s.SyncChangesets(ctx, cs)
}

// SyncChangesets refreshes the metadata of the given changesets and
// updates them in the database
func (s *ChangesetSyncer) SyncChangesets(ctx context.Context, cs ...*campaigns.Changeset) (err error) {
	if len(cs) == 0 {
		return nil
	}

	bySource, err := s.GroupChangesetsBySource(ctx, cs...)
	if err != nil {
		return err
	}

	return s.SyncChangesetsWithSources(ctx, bySource)
}

// SyncChangesetsWithSources refreshes the metadata of the given changesets
// with the given ChangesetSources and updates them in the database.
func (s *ChangesetSyncer) SyncChangesetsWithSources(ctx context.Context, bySource []*SourceChangesets) (err error) {
	var (
		events []*campaigns.ChangesetEvent
		cs     []*campaigns.Changeset
	)

	for _, s := range bySource {
		var notFound []*repos.Changeset

		err := s.LoadChangesets(ctx, s.Changesets...)
		if err != nil {
			notFoundErr, ok := err.(repos.ChangesetsNotFoundError)
			if !ok {
				return err
			}
			notFound = notFoundErr.Changesets
		}

		notFoundById := make(map[int64]*repos.Changeset, len(notFound))
		for _, c := range notFound {
			notFoundById[c.Changeset.ID] = c
		}

		for _, c := range s.Changesets {
			_, notFound := notFoundById[c.Changeset.ID]
			if notFound && !c.Changeset.IsDeleted() {
				c.Changeset.SetDeleted()
			}

			events = append(events, c.Events()...)
			cs = append(cs, c.Changeset)
		}
	}

	tx, err := s.Store.Transact(ctx)
	if err != nil {
		return err
	}
	defer tx.Done(&err)

	if err = tx.UpdateChangesets(ctx, cs...); err != nil {
		return err
	}

	return tx.UpsertChangesetEvents(ctx, events...)
}

// GroupChangesetsBySource returns a slice of SourceChangesets in which the
// given *campaigns.Changesets are grouped together as repos.Changesets with the
// repos.Source that can modify them.
func (s *ChangesetSyncer) GroupChangesetsBySource(ctx context.Context, cs ...*campaigns.Changeset) ([]*SourceChangesets, error) {
	var repoIDs []api.RepoID
	repoSet := map[api.RepoID]*repos.Repo{}

	for _, c := range cs {
		id := c.RepoID
		if _, ok := repoSet[id]; !ok {
			repoSet[id] = nil
			repoIDs = append(repoIDs, id)
		}
	}

	rs, err := s.ReposStore.ListRepos(ctx, repos.StoreListReposArgs{IDs: repoIDs})
	if err != nil {
		return nil, err
	}

	for _, r := range rs {
		repoSet[r.ID] = r
	}

	for _, c := range cs {
		repo := repoSet[c.RepoID]
		if repo == nil {
			log15.Warn("changeset not synced, repo not in database", "changeset_id", c.ID, "repo_id", c.RepoID)
		}
	}

	es, err := s.ReposStore.ListExternalServices(ctx, repos.StoreListExternalServicesArgs{RepoIDs: repoIDs})
	if err != nil {
		return nil, err
	}

	byRepo := make(map[api.RepoID]int64, len(rs))
	for _, r := range rs {
		eids := r.ExternalServiceIDs()
		for _, id := range eids {
			if _, ok := byRepo[r.ID]; !ok {
				byRepo[r.ID] = id
				break
			}
		}
	}

	bySource := make(map[int64]*SourceChangesets, len(es))
	for _, e := range es {
		src, err := repos.NewSource(e, s.HTTPFactory)
		if err != nil {
			return nil, err
		}

		css, ok := src.(repos.ChangesetSource)
		if !ok {
			return nil, errors.Errorf("unsupported repo type %q", e.Kind)
		}

		bySource[e.ID] = &SourceChangesets{ChangesetSource: css}
	}

	for _, c := range cs {
		repoID := c.RepoID
		s := bySource[byRepo[repoID]]
		if s == nil {
			continue
		}
		s.Changesets = append(s.Changesets, &repos.Changeset{
			Changeset: c,
			Repo:      repoSet[repoID],
		})
	}

	res := make([]*SourceChangesets, 0, len(bySource))
	for _, s := range bySource {
		res = append(res, s)
	}

	return res, nil
}

func (s *ChangesetSyncer) listAllNonDeletedChangesets(ctx context.Context) (all []*campaigns.Changeset, err error) {
	for cursor := int64(-1); cursor != 0; {
		opts := ListChangesetsOpts{
			Cursor:         cursor,
			Limit:          1000,
			WithoutDeleted: true,
		}
		cs, next, err := s.Store.ListChangesets(ctx, opts)
		if err != nil {
			return nil, err
		}
		all, cursor = append(all, cs...), next
	}

	return all, err
}

type syncSchedule struct {
	changesetID int64
	nextSync    time.Time
	priority    priority
}

// changesetPriorityQueue is a min heap that sorts syncs by priority
// and time of next sync
// It is not safe for concurrent use so callers should protect it with
// a mutex
type changesetPriorityQueue struct {
	items []*syncSchedule
	index map[int64]int // changesetID -> index
}

func newChangesetPriorityQueue() *changesetPriorityQueue {
	q := &changesetPriorityQueue{
		items: make([]*syncSchedule, 0),
		index: make(map[int64]int),
	}
	heap.Init(q)
	return q
}

func (pq *changesetPriorityQueue) Len() int { return len(pq.items) }

func (pq *changesetPriorityQueue) Less(i, j int) bool {
	// We want items ordered by priority, then nextSync
	// Order by priority and then nextSync
	a := pq.items[i]
	b := pq.items[j]

	if a.priority != b.priority {
		// Greater than here since we want high priority items to be ranked before low priority
		return a.priority > b.priority
	}
	if !a.nextSync.Equal(b.nextSync) {
		return a.nextSync.Before(b.nextSync)
	}
	return a.changesetID < b.changesetID
}

func (pq *changesetPriorityQueue) Swap(i, j int) {
	pq.items[i], pq.items[j] = pq.items[j], pq.items[i]
	pq.index[pq.items[i].changesetID] = i
	pq.index[pq.items[j].changesetID] = j
}

// Push is here to implement the Heap interface, please use Upsert instead
func (pq *changesetPriorityQueue) Push(x interface{}) {
	n := len(pq.items)
	item := x.(*syncSchedule)
	pq.index[item.changesetID] = n
	pq.items = append(pq.items, item)
}

func (pq *changesetPriorityQueue) Pop() interface{} {
	old := pq.items
	n := len(old)
	item := old[n-1]
	old[n-1] = nil // avoid memory leak
	delete(pq.index, item.changesetID)
	pq.items = old[0 : n-1]
	return item
}

// Upsert modifies at item if it exists or adds a new item
// NOTE: If an existing item is high priority, it will not be changed back
// to normal. This allows high priority items to stay that way reschedules
func (pq *changesetPriorityQueue) Upsert(s *syncSchedule) {
	i, ok := pq.index[s.changesetID]
	if !ok {
		heap.Push(pq, s)
		return
	}
	item := pq.items[i]
	oldPriority := item.priority
	*item = *s
	if oldPriority == priorityHigh {
		item.priority = priorityHigh
	}
	heap.Fix(pq, i)
}

func (pq *changesetPriorityQueue) Get(id int64) (*syncSchedule, bool) {
	i, ok := pq.index[id]
	if !ok {
		return nil, false
	}
	item := pq.items[i]
	return item, true
}

func (pq *changesetPriorityQueue) Reschedule(ss []syncSchedule) {
	for i := range ss {
		pq.Upsert(&ss[i])
	}
}

type priority int

const (
	priorityNormal priority = iota
	priorityHigh   priority = iota
)

// A SourceChangesets groups *repos.Changesets together with the
// repos.ChangesetSource that can be used to modify the changesets.
type SourceChangesets struct {
	repos.ChangesetSource
	Changesets []*repos.Changeset
}
