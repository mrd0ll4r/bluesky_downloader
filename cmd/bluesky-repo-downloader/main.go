package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/backfill"
	did2 "github.com/bluesky-social/indigo/did"
	lexutil "github.com/bluesky-social/indigo/lex/util"
	"github.com/bluesky-social/indigo/repo"
	"github.com/bluesky-social/indigo/xrpc"
	"github.com/ipfs/go-cid"
	"github.com/labstack/gommon/log"
	typegen "github.com/whyrusleeping/cbor-gen"
	"go.opentelemetry.io/otel"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"log/slog"
	"os"
	"os/signal"
	"path"
	"strings"
	"time"
)

var (
	USER_AGENT         = "github.com/mrd0ll4r/bluesky_downloader/repo_downloader"
	ENV_REPO_DISCOVERY = "ENABLE_REPO_DISCOVERY"
	OUTPUT_DIR         = "repos"
	POSTGRES_DSN       = "postgres://bluesky_indexer:bluesky_indexer@localhost:5434/bluesky_indexer"
)

func main() {
	server, err := doStuff()
	if err != nil {
		panic(err)
	}

	go func() {
		err := server.Run(context.Background())
		if err != nil {
			panic(err)
		}
	}()

	// Set up channel on which to send signal notifications.
	// We must use a buffered channel or risk missing the signal
	// if we're not ready to receive when the signal is sent.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	fmt.Println("Press Ctrl-C to stop")

	// Block until a signal is received.
	<-c

	fmt.Println("Shutting down...")
	r := make(chan error)
	go func() {
		err = server.Stop(context.Background())
		if err != nil {
			r <- err
		}
		close(r)
	}()

	select {
	case <-c:
		// Another signal, just die
		fmt.Println("Shutting down now.")
	case err = <-r:
		// We're done shutting down
		if err != nil {
			panic(err)
		}
	}
}

func setupDB() (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(POSTGRES_DSN), &gorm.Config{
		SkipDefaultTransaction: true,
		TranslateError:         true,
	})
	if err != nil {
		return nil, err
	}

	sqldb, err := db.DB()
	if err != nil {
		return nil, err
	}

	sqldb.SetMaxIdleConns(80)
	sqldb.SetMaxOpenConns(40)
	sqldb.SetConnMaxIdleTime(time.Hour)

	return db, nil
}

func doStuff() (*Server, error) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	db, err := setupDB()
	if err != nil {
		return nil, err
	}

	logger.Info("running database migrations")
	err = db.AutoMigrate(&backfill.GormDBJob{})
	if err != nil {
		return nil, err
	}

	writer, err := NewRepoWriter(OUTPUT_DIR, 4, logger, 4)
	if err != nil {
		return nil, fmt.Errorf("unable to set up repo writer: %w", err)
	}

	bgsws := "wss://bsky.network"
	if !strings.HasPrefix(bgsws, "ws") {
		return nil, fmt.Errorf("specified bgs host must include 'ws://' or 'wss://'")
	}

	bgshttp := strings.Replace(bgsws, "ws", "http", 1)
	bgsxrpc := &xrpc.Client{
		Host: bgshttp,
	}
	bgsxrpc.UserAgent = &USER_AGENT

	_, enableRepoDiscovery := os.LookupEnv(ENV_REPO_DISCOVERY)

	bfstore := backfill.NewGormstore(db)
	opts := DefaultBackfillOptions()

	// Adjust some rate limits.
	// In theory, the limits should 3000/5m = 10/s, so 8 should be fine.
	opts.SyncRequestsPerSecond = 8
	opts.ParallelBackfills = 16

	// TODO maybe remove this to see if there's other stuff on ATProto?
	opts.NSIDFilter = "app.bsky."
	bf := NewBackfiller(
		"repo-downloader",
		bfstore,
		bgsxrpc,
		opts,
		writer,
	)

	s := &Server{
		bgsxrpc:             bgsxrpc,
		logger:              logger,
		bfs:                 bfstore,
		bf:                  bf,
		writer:              writer,
		enableRepoDiscovery: enableRepoDiscovery,
		stop:                make(chan struct{}),
	}

	return s, nil
}

type Server struct {
	bgsxrpc *xrpc.Client
	logger  *slog.Logger

	bfs    *backfill.Gormstore
	bf     *Backfiller
	writer *RepoWriter
	stop   chan struct{}

	enableRepoDiscovery bool
}

func (s *Server) Stop(ctx context.Context) error {
	s.logger.Info("stopping repo discovery")
	s.stop <- struct{}{}

	err := s.bf.Stop(ctx)
	if err != nil {
		return fmt.Errorf("unable to stop backfiller: %w", err)
	}

	err = s.writer.Stop(ctx)
	if err != nil {
		return fmt.Errorf("unable to stop writer: %w", err)
	}

	return nil
}

func (s *Server) Run(ctx context.Context) error {
	err := s.bfs.LoadJobs(ctx)
	if err != nil {
		return fmt.Errorf("loading backfill jobs: %w", err)
	}

	if s.enableRepoDiscovery {
		s.logger.Info("repo discovery enabled")
		go s.discoverRepos()
	} else {
		s.logger.Warn("repo discovery turned OFF. Only processing already-known jobs")
	}

	go s.writer.Run(ctx)

	go s.bf.Run()

	return nil
}

func (s *Server) discoverRepos() {
	ctx := context.Background()
	log := s.logger.With("func", "discoverRepos")
	log.Info("starting repo discovery")

	cursor := ""
	limit := int64(500)

	total := 0
	totalErrored := 0

	for {
		select {
		case <-s.stop:
			s.logger.Info("quitting repo discovery")
			return
		default:
		}
		resp, err := atproto.SyncListRepos(ctx, s.bgsxrpc, cursor, limit)
		if err != nil {
			log.Error("failed to list repos", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Info("got repo page", "count", len(resp.Repos), "cursor", resp.Cursor)
		errored := 0
		for _, r := range resp.Repos {
			// Create a job if not already present.
			_, err := s.bfs.GetOrCreateJob(ctx, r.Did, StateEnqueued)
			if err != nil {
				log.Error("failed to get or create job", "did", r.Did, "err", err)
				errored++
				continue
			}

			// TODO this was not in the original, but we could do this to force
			// repos to be re-downloaded on every run.
			/*
				err = j.SetState(ctx, StateEnqueued)
				if err != nil {
					log.Error("failed to get or create job", "did", r.Did, "err", err)
					errored++
				}
			*/
		}
		log.Info("enqueued repos", "total", len(resp.Repos), "errored", errored)
		totalErrored += errored
		total += len(resp.Repos)
		if resp.Cursor != nil && *resp.Cursor != "" {
			cursor = *resp.Cursor
		} else {
			break
		}
	}

	log.Info("finished repo discovery", "totalJobs", total, "totalErrored", totalErrored)
}

type RepoWriter struct {
	baseDir        string
	didSplitLength int
	reposIn        chan repoWriterJob
	logger         *slog.Logger

	parallelWrites int

	stop chan chan struct{}
}

func NewRepoWriter(baseDir string, didSplitLength int, logger *slog.Logger, parallelWrites int) (*RepoWriter, error) {
	err := os.MkdirAll(baseDir, 0777)
	if err != nil {
		return nil, fmt.Errorf("unable to mkdir: %w", err)
	}

	w := &RepoWriter{
		baseDir:        baseDir,
		didSplitLength: didSplitLength,
		reposIn:        make(chan repoWriterJob),
		logger:         logger,
		parallelWrites: parallelWrites,
		stop:           make(chan chan struct{}, 1),
	}

	return w, nil
}

func (w *RepoWriter) Stop(ctx context.Context) error {
	w.logger.Info("stopping writer")
	stopped := make(chan struct{})
	w.stop <- stopped
	select {
	case <-stopped:
		w.logger.Info("writer stopped")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *RepoWriter) Run(ctx context.Context) {
	sem := semaphore.NewWeighted(int64(w.parallelWrites))

	for {
		select {
		case stopped := <-w.stop:
			log.Info("stopping writer")
			sem.Acquire(ctx, int64(w.parallelWrites))
			close(stopped)
			return
		default:
		}

		select {
		case job := <-w.reposIn:
			sem.Acquire(ctx, 1)
			go func(j repoWriterJob) {
				defer sem.Release(1)

				err := w.doWork(j.repoRev)
				if err != nil {
					w.logger.Error("unable to write repo", "err", err)
					j.res <- err
				}
				close(j.res)
			}(job)
		default:
			// No work, sleep
			time.Sleep(1 * time.Second)
		}

	}
}

func (w *RepoWriter) calculatePathAndOpenFile(did string, rev string) (*os.File, *os.File, error) {
	parsedDid, err := did2.ParseDID(did)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid DID: %w", err)
	}

	// Split DID into parts to not create a few million directory entries...
	repoDir := ""
	if len(parsedDid.Value()) > w.didSplitLength {
		split1 := parsedDid.Value()[:w.didSplitLength]
		split2 := parsedDid.Value()[w.didSplitLength:]

		repoDir = path.Join(w.baseDir, "did", parsedDid.Protocol(), split1, split2, "repo_revisions")
	} else {
		repoDir = path.Join(w.baseDir, "did", parsedDid.Protocol(), parsedDid.Value(), "repo_revisions")
	}

	err = os.MkdirAll(repoDir, 0777)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to mkdir: %w", err)
	}

	repoJsonFile := path.Join(repoDir, fmt.Sprintf("%s.json.gz", rev))
	jsonFile, err := os.Create(repoJsonFile)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create JSON repo file: %w", err)
	}

	repoCarFile := path.Join(repoDir, fmt.Sprintf("%s.car.gz", rev))
	carFile, err := os.Create(repoCarFile)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create CAR repo file: %w", err)
	}

	return jsonFile, carFile, nil
}

type JsonRepoEntry struct {
	Path         string      `json:"path"`
	RawBlock     []byte      `json:"raw_block"`
	DecodedBlock interface{} `json:"decoded_block"`
	DecodeError  *string     `json:"decode_error"`
	Cid          cid.Cid     `json:"cid"`
}

type JsonRepoRevision struct {
	Did      string          `json:"did"`
	Revision string          `json:"revision"`
	Entries  []JsonRepoEntry `json:"entries"`
}

func (w *RepoWriter) doWork(rev *repoRevision) error {
	jsonF, carF, err := w.calculatePathAndOpenFile(rev.did, rev.rev)
	if err != nil {
		return fmt.Errorf("unable to create repo file: %w", err)
	}
	defer jsonF.Close()
	defer carF.Close()

	// Write the CAR file first.
	// In case something goes wrong with all the JSON encoding/decoding below,
	// we'll at least have this.
	carGzipWriter, err := gzip.NewWriterLevel(carF, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("unable to create gzip writer: %w", err)
	}
	defer carGzipWriter.Close()

	n, err := carGzipWriter.Write(rev.rawCar)
	if err != nil {
		return fmt.Errorf("unable to write revision CAR: %w", err)
	}
	if n != len(rev.rawCar) {
		return fmt.Errorf("did not write the entire car file, expected to write %d byte, wrote %d byte", len(rev.rawCar), n)
	}

	prettyRevision := JsonRepoRevision{
		Revision: rev.rev,
		Did:      rev.did,
	}

	for _, entry := range rev.entries {
		decodedBlock, err := translateRecord(entry.rawBlock)
		var errString *string
		if err != nil {
			w.logger.Warn("unable to translate block to JSON", "did", rev.did, "rev", rev.rev, "err", err, "path", entry.path)
			tmp := err.Error()
			errString = &tmp
		}

		translatedEntry := JsonRepoEntry{
			Path:         entry.path,
			RawBlock:     entry.rawBlock,
			DecodedBlock: decodedBlock,
			DecodeError:  errString,
			Cid:          entry.cid,
		}

		prettyRevision.Entries = append(prettyRevision.Entries, translatedEntry)
	}

	jsonGzipWriter, err := gzip.NewWriterLevel(jsonF, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("unable to create gzip writer: %w", err)
	}
	defer jsonGzipWriter.Close()

	enc := json.NewEncoder(jsonGzipWriter)
	err = enc.Encode(prettyRevision)
	if err != nil {
		return fmt.Errorf("unable to write JSON to file: %w", err)
	}

	return nil
}

func translateRecord(recB []byte) (interface{}, error) {
	// CBOR Unmarshal the record
	recCBOR, err := lexutil.CborDecodeValue(recB)
	if err != nil {
		return nil, fmt.Errorf("cbor decode: %w", err)
	}

	// Re-marshal as JSON.
	rec, ok := recCBOR.(typegen.CBORMarshaler)
	if !ok {
		return nil, fmt.Errorf("failed to cast record to CBORMarshaler")
	}
	d := lexutil.LexiconTypeDecoder{Val: rec}
	b, err := d.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("unable to marshal as JSON: %w", err)
	}

	// Now un-marshal that again to get a marshal-able type to marshal (later).
	var m map[string]interface{}
	err = json.Unmarshal(b, &m)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal as JSON: %w", err)
	}

	return m, nil
}

type repoWriterJob struct {
	repoRev *repoRevision
	res     chan error
}

// Backfiller is a struct which handles backfilling a repo
type Backfiller struct {
	Name  string
	Store backfill.Store

	// Number of backfills to process in parallel
	ParallelBackfills int
	// Prefix match for records to backfill i.e. app.bsky.feed.app/
	// If empty, all records will be backfilled
	NSIDFilter string
	// The client to use for requests
	bgsxrpc *xrpc.Client

	syncLimiter *rate.Limiter

	stop chan chan struct{}

	writerChan chan repoWriterJob
}

var (
	// StateEnqueued is the state of a backfill job when it is first created
	StateEnqueued = "enqueued"
	// StateInProgress is the state of a backfill job when it is being processed
	StateInProgress = "in_progress"
	// StateComplete is the state of a backfill job when it has been processed
	StateComplete = "complete"
)

var tracer = otel.Tracer("backfiller")

type BackfillOptions struct {
	ParallelBackfills     int
	NSIDFilter            string
	SyncRequestsPerSecond int
}

func DefaultBackfillOptions() *BackfillOptions {
	return &BackfillOptions{
		ParallelBackfills:     10,
		NSIDFilter:            "",
		SyncRequestsPerSecond: 2,
	}
}

// NewBackfiller creates a new Backfiller
func NewBackfiller(
	name string,
	store backfill.Store,
	bgsrpc *xrpc.Client,
	opts *BackfillOptions,
	writer *RepoWriter,
) *Backfiller {
	if opts == nil {
		opts = DefaultBackfillOptions()
	}
	return &Backfiller{
		Name:              name,
		Store:             store,
		ParallelBackfills: opts.ParallelBackfills,
		NSIDFilter:        opts.NSIDFilter,
		bgsxrpc:           bgsrpc,
		syncLimiter:       rate.NewLimiter(rate.Limit(opts.SyncRequestsPerSecond), 1),
		stop:              make(chan chan struct{}, 1),
		writerChan:        writer.reposIn,
	}
}

// Run starts the backfill processor routine
func (b *Backfiller) Run() {
	ctx := context.Background()

	log := slog.With("source", "backfiller", "name", b.Name)
	log.Info("starting backfill processor")

	sem := semaphore.NewWeighted(int64(b.ParallelBackfills))

	for {
		select {
		case stopped := <-b.stop:
			log.Info("stopping backfill processor")
			sem.Acquire(ctx, int64(b.ParallelBackfills))
			close(stopped)
			return
		default:
		}

		// Get the next job
		job, err := b.Store.GetNextEnqueuedJob(ctx)
		if err != nil {
			log.Error("failed to get next enqueued job", "error", err)
			time.Sleep(1 * time.Second)
			continue
		} else if job == nil {
			time.Sleep(1 * time.Second)
			continue
		}

		log := log.With("repo", job.Repo())

		// Mark the backfill as "in progress"
		err = job.SetState(ctx, StateInProgress)
		if err != nil {
			log.Error("failed to set job state", "error", err)
			continue
		}

		sem.Acquire(ctx, 1)
		go func(j backfill.Job) {
			defer sem.Release(1)
			newState, err := b.BackfillRepo(ctx, j)
			if err != nil {
				log.Error("failed to backfill repo", "error", err)
			}
			if newState != "" {
				if sserr := j.SetState(ctx, newState); sserr != nil {
					log.Error("failed to set job state", "error", sserr)
				}

				if strings.HasPrefix(newState, "failed") {
					// Clear buffered ops
					if err := j.ClearBufferedOps(ctx); err != nil {
						log.Error("failed to clear buffered ops", "error", err)
					}
				}
			}
		}(job)
	}
}

// Stop stops the backfill processor
func (b *Backfiller) Stop(ctx context.Context) error {
	log := slog.With("source", "backfiller", "name", b.Name)
	log.Info("stopping backfill processor")
	stopped := make(chan struct{})
	b.stop <- stopped
	select {
	case <-stopped:
		log.Info("backfill processor stopped")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// BackfillRepo backfills a repo
func (b *Backfiller) BackfillRepo(ctx context.Context, job backfill.Job) (string, error) {
	ctx, span := tracer.Start(ctx, "BackfillRepo")
	defer span.End()

	start := time.Now()

	repoDid := job.Repo()

	log := slog.With("source", "backfiller_backfill_repo", "repo", repoDid)
	if job.RetryCount() > 0 {
		log = log.With("retry_count", job.RetryCount())
	}
	log.Info(fmt.Sprintf("processing backfill for %s", repoDid))

	b.syncLimiter.Wait(ctx)

	resp, err := atproto.SyncGetRepo(ctx, b.bgsxrpc, repoDid, job.Rev())
	if err != nil {
		state := fmt.Sprintf("failed (do request: %s)", err.Error())
		return state, fmt.Errorf("failed to send request: %w", err)
	}

	r, err := repo.ReadRepoFromCar(ctx, bytes.NewReader(resp))
	if err != nil {
		state := "failed (couldn't read repo CAR from response body)"
		return state, fmt.Errorf("failed to read repo from car: %w", err)
	}

	// TODO if we supplied a revision to create a diff from, this will not be
	// a complete repo, but just a diff, maybe?

	numRecords := 0
	rev := r.SignedCommit().Rev
	repoRev := repoRevision{
		rev:    rev,
		rawCar: resp,
		did:    repoDid,
	}

	if err := r.ForEach(ctx, b.NSIDFilter, func(recordPath string, nodeCid cid.Cid) error {
		numRecords++

		blk, err := r.Blockstore().Get(ctx, nodeCid)
		if err != nil {
			return fmt.Errorf("unable to get block for repo entry %s (%s): %w", nodeCid, recordPath, err)
		}

		raw := blk.RawData()

		repoRev.entries = append(repoRev.entries, repoEntry{
			path:     recordPath,
			rawBlock: raw,
			cid:      nodeCid,
		})

		return nil
	}); err != nil {
		state := fmt.Sprintf("failed (read repo: %s)", err.Error())
		return state, fmt.Errorf("failed to read repo: %w", err)
	}

	// Push to writer, see what comes out of that
	resChan := make(chan error)
	b.writerChan <- repoWriterJob{repoRev: &repoRev, res: resChan}
	err = <-resChan
	if err != nil {
		state := fmt.Sprintf("failed (write: %s)", err.Error())
		return state, fmt.Errorf("failed to write to disk: %w", err)
	}

	if err := job.SetRev(ctx, r.SignedCommit().Rev); err != nil {
		state := fmt.Sprintf("failed (set rev: %s)", err.Error())
		return state, fmt.Errorf("failed to set job rev: %w", err)
	}

	log.Info("backfill complete",
		"records_backfilled", numRecords,
		"duration", time.Since(start),
	)

	return StateComplete, nil
}

type repoEntry struct {
	path     string
	rawBlock []byte
	cid      cid.Cid
}

type repoRevision struct {
	did     string
	rev     string
	rawCar  []byte
	entries []repoEntry
}
