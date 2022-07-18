package codeownership

import (
	"context"

	otlog "github.com/opentracing/opentracing-go/log"

	"github.com/sourcegraph/sourcegraph/internal/search"
	"github.com/sourcegraph/sourcegraph/internal/search/job"
	"github.com/sourcegraph/sourcegraph/internal/search/streaming"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

func New(child job.Job, fileOwnersMustInclude []string, fileOwnersMustExclude []string) job.Job {
	return &filterJob{
		child:                 child,
		fileOwnersMustInclude: fileOwnersMustInclude,
		fileOwnersMustExclude: fileOwnersMustExclude,
	}
}

type filterJob struct {
	child job.Job

	fileOwnersMustInclude []string
	fileOwnersMustExclude []string
}

func (s *filterJob) Run(ctx context.Context, clients job.RuntimeClients, stream streaming.Sender) (alert *search.Alert, err error) {
	_, ctx, stream, finish := job.StartSpan(ctx, stream, s)
	defer func() { finish(alert, err) }()

	var errs error

	// We currently don't have a way to access file ownership information, so no
	// file currently has any owner. A search to include an owner will therefore
	// return no results.
	filteredStream := streaming.StreamFunc(func(event streaming.SearchEvent) {
		if len(s.fileOwnersMustExclude) > 0 {
			event.Results = event.Results[:0]
		}
		stream.Send(event)
	})

	alert, err = s.child.Run(ctx, clients, filteredStream)
	if err != nil {
		errs = errors.Append(errs, err)
	}
	return alert, errs
}

func (s *filterJob) Name() string {
	return "codeownershipFilterJob"
}

func (s *filterJob) Fields(job.Verbosity) []otlog.Field { return nil }

func (s *filterJob) Children() []job.Describer {
	return []job.Describer{s.child}
}

func (s *filterJob) MapChildren(fn job.MapFunc) job.Job {
	cp := *s
	cp.child = job.Map(s.child, fn)
	return &cp
}
