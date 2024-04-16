package backfillmod

import (
	"context"
	"errors"
	"fmt"
	"github.com/bluesky-social/indigo/backfill"
	"github.com/bluesky-social/indigo/repomgr"
	"github.com/ipfs/go-cid"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

type Gormjob struct {
	lk sync.Mutex

	dbj *GormDBJob
	db  *gorm.DB
}

func (j *Gormjob) BufferOps(ctx context.Context, since *string, rev string, ops []*backfill.BufferedOp) (bool, error) {
	//TODO implement me
	panic("not implemented")
}

func (j *Gormjob) FlushBufferedOps(ctx context.Context, cb func(kind repomgr.EventKind, rev string, path string, rec *[]byte, cid *cid.Cid) error) error {
	//TODO implement me
	// NOP
	return nil
}

func (j *Gormjob) ClearBufferedOps(ctx context.Context) error {
	//TODO implement me
	// NOP
	return nil
}

type GormDBJob struct {
	gorm.Model
	Repo       string `gorm:"unique;index"`
	State      string `gorm:"index"`
	Rev        string
	RetryCount int
	RetryAfter *time.Time
}

// Gormstore is a gorm-backed implementation of the Backfill Store interface.
// This is a clone of indigo/backfill/gormstore.go, with some modifications.
type Gormstore struct {
	lk   sync.RWMutex
	jobs map[string]*Gormjob

	queueLock sync.Mutex
	taskQueue []string

	db *gorm.DB
}

func NewGormstore(db *gorm.DB) *Gormstore {
	return &Gormstore{
		jobs: make(map[string]*Gormjob),
		db:   db,
	}
}

func (s *Gormstore) LoadJobs(ctx context.Context) error {
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	return s.loadJobs(ctx, 20_000)
}

func (s *Gormstore) loadJobs(ctx context.Context, limit int) error {
	var todo []string
	if err := s.db.Model(GormDBJob{}).Limit(limit).Select("repo").
		Where("state = 'enqueued' OR (state like 'failed%' AND (retry_after = NULL OR retry_after < ?))", time.Now()).Scan(&todo).Error; err != nil {
		return err
	}

	s.taskQueue = append(s.taskQueue, todo...)

	return nil
}

func (s *Gormstore) GetOrCreateJob(ctx context.Context, repo, state string) (backfill.Job, error) {
	j, err := s.getJob(ctx, repo)
	if err == nil {
		return j, nil
	}

	if !errors.Is(err, backfill.ErrJobNotFound) {
		return nil, err
	}

	if err := s.createJobForRepo(repo, state); err != nil {
		return nil, err
	}

	return s.getJob(ctx, repo)
}

func (s *Gormstore) EnqueueJob(ctx context.Context, repo string) error {
	return s.EnqueueJobWithState(ctx, repo, backfill.StateEnqueued)
}

func (s *Gormstore) EnqueueJobWithState(ctx context.Context, repo, state string) error {
	_, err := s.GetOrCreateJob(ctx, repo, state)
	if err != nil {
		return err
	}

	s.queueLock.Lock()
	s.taskQueue = append(s.taskQueue, repo)
	s.queueLock.Unlock()

	return nil
}

func (s *Gormstore) createJobForRepo(repo, state string) error {
	dbj := &GormDBJob{
		Repo:  repo,
		State: state,
	}
	if err := s.db.Create(dbj).Error; err != nil {
		if err == gorm.ErrDuplicatedKey {
			return nil
		}
		return err
	}

	s.lk.Lock()
	defer s.lk.Unlock()

	// Convert it to an in-memory job
	if _, ok := s.jobs[repo]; ok {
		// The DB create should have errored if the job already existed, but just in case
		return fmt.Errorf("job already exists for repo %s", repo)
	}

	j := &Gormjob{
		dbj: dbj,
		db:  s.db,
	}
	s.jobs[repo] = j

	return nil
}

func (s *Gormstore) GetJob(ctx context.Context, repo string) (backfill.Job, error) {
	return s.getJob(ctx, repo)
}

func (s *Gormstore) getJob(ctx context.Context, repo string) (*Gormjob, error) {
	cj := s.checkJobCache(ctx, repo)
	if cj != nil {
		return cj, nil
	}

	return s.loadJob(ctx, repo)
}

func (s *Gormstore) loadJob(ctx context.Context, repo string) (*Gormjob, error) {
	var dbj GormDBJob
	if err := s.db.Find(&dbj, "repo = ?", repo).Error; err != nil {
		return nil, err
	}

	if dbj.ID == 0 {
		return nil, backfill.ErrJobNotFound
	}

	j := &Gormjob{
		dbj: &dbj,
		db:  s.db,
	}
	s.lk.Lock()
	defer s.lk.Unlock()
	// would imply a race condition
	exist, ok := s.jobs[repo]
	if ok {
		return exist, nil
	}
	s.jobs[repo] = j
	return j, nil
}

func (s *Gormstore) checkJobCache(ctx context.Context, repo string) *Gormjob {
	s.lk.RLock()
	defer s.lk.RUnlock()

	j, ok := s.jobs[repo]
	if !ok || j == nil {
		return nil
	}
	return j
}

func (s *Gormstore) GetNextEnqueuedJob(ctx context.Context) (backfill.Job, error) {
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	if len(s.taskQueue) == 0 {
		if err := s.loadJobs(ctx, 1000); err != nil {
			return nil, err
		}

		if len(s.taskQueue) == 0 {
			return nil, nil
		}
	}

	for len(s.taskQueue) > 0 {
		first := s.taskQueue[0]
		s.taskQueue = s.taskQueue[1:]

		j, err := s.getJob(ctx, first)
		if err != nil {
			return nil, err
		}

		shouldRetry := strings.HasPrefix(j.State(), "failed") && j.dbj.RetryAfter != nil && time.Now().After(*j.dbj.RetryAfter)

		if j.State() == backfill.StateEnqueued || shouldRetry {
			return j, nil
		}
	}
	return nil, nil
}

func (j *Gormjob) Repo() string {
	return j.dbj.Repo
}

func (j *Gormjob) State() string {
	j.lk.Lock()
	defer j.lk.Unlock()

	return j.dbj.State
}

func (j *Gormjob) SetRev(ctx context.Context, r string) error {
	j.lk.Lock()
	defer j.lk.Unlock()

	j.dbj.Rev = r

	// Persist the job to the database
	return j.db.Save(j.dbj).Error
}

func (j *Gormjob) Rev() string {
	j.lk.Lock()
	defer j.lk.Unlock()

	return j.dbj.Rev
}
func computeExponentialBackoff(attempt int) time.Duration {
	return time.Duration(1<<uint(attempt)) * 10 * time.Second
}

func (j *Gormjob) SetState(ctx context.Context, state string) error {
	j.lk.Lock()
	defer j.lk.Unlock()

	j.dbj.State = state

	if strings.HasPrefix(state, "failed") {
		if j.dbj.RetryCount < backfill.MaxRetries {
			next := time.Now().Add(computeExponentialBackoff(j.dbj.RetryCount))
			j.dbj.RetryAfter = &next
			j.dbj.RetryCount++
		} else {
			j.dbj.RetryAfter = nil
		}
	}

	// Persist the job to the database
	j.dbj.State = state
	return j.db.Save(j.dbj).Error
}

func (j *Gormjob) RetryCount() int {
	j.lk.Lock()
	defer j.lk.Unlock()
	return j.dbj.RetryCount
}

func (s *Gormstore) UpdateRev(ctx context.Context, repo, rev string) error {
	j, err := s.GetJob(ctx, repo)
	if err != nil {
		return err
	}

	return j.SetRev(ctx, rev)
}

func (s *Gormstore) PurgeRepo(ctx context.Context, repo string) error {
	if err := s.db.Exec("DELETE FROM gorm_db_jobs WHERE repo = ?", repo).Error; err != nil {
		return err
	}

	s.lk.Lock()
	defer s.lk.Unlock()
	delete(s.jobs, repo)

	return nil
}
