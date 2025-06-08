package main

import (
	"bluesky-downloader/event"
	"context"
	"errors"
	"fmt"
	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/events"
	"github.com/bluesky-social/indigo/events/schedulers/sequential"
	"github.com/gorilla/websocket"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	flag "github.com/spf13/pflag"
)

var (
	USER_AGENT                  = "github.com/mrd0ll4r/bluesky_downloader/firehose_logger"
	DEFAULT_OUTPUT_DIR          = "firehose_logs"
	DEFAULT_FIREHOSE_REMOTE     = "wss://bsky.network"
	DEFAULT_NUM_EVENTS_PER_FILE = uint(10_000)
	DEFAULT_POSTGRES_DSN        = "postgres://bluesky_indexer:bluesky_indexer@localhost:5434/bluesky_indexer"
)

// Every how many events should we persist the cursor to the database?
const SEQUENCE_NUMBER_PERSISTENCE_INTERVAL int64 = 500

type Subscriber struct {
	db                   *gorm.DB
	bgsUrl               url.URL
	logger               *slog.Logger
	writer               *event.RotatingWriter
	saveRepoCommitBlocks bool
	resetCursor          bool
	closed               chan error
}

type LastSeq struct {
	ID  uint `gorm:"primarykey"`
	Seq int64
}

func (s *Subscriber) getLastCursor() (int64, error) {
	var lastSeq LastSeq
	if err := s.db.Find(&lastSeq).Error; err != nil {
		return 0, err
	}

	if lastSeq.ID == 0 {
		return 0, s.db.Create(&lastSeq).Error
	}

	return lastSeq.Seq, nil
}

func (s *Subscriber) updateLastCursor(curs int64) error {
	// TODO I think we need to +1 this?
	// It seems the firehose returns events with seqNo >= since.
	return s.db.Model(LastSeq{}).Where("id = 1").Update("seq", curs+1).Error
}

func (s *Subscriber) Run(ctx context.Context, livenessChan chan struct{}) error {
	defer func() { close(s.closed) }()
	lastCur, err := s.getLastCursor()
	if err != nil {
		return fmt.Errorf("get last cursor: %w", err)
	}
	if s.resetCursor {
		s.logger.Info("resetting cursor")
		lastCur = 0
	}

	d := websocket.DefaultDialer
	u := s.bgsUrl
	u.Scheme = "wss"
	u.Path = "/xrpc/com.atproto.sync.subscribeRepos"
	if lastCur != 0 {
		u.RawQuery = fmt.Sprintf("cursor=%d", lastCur)
	}
	con, _, err := d.Dial(u.String(), http.Header{
		"User-Agent": []string{USER_AGENT},
	})
	if err != nil {
		return fmt.Errorf("events dial failed: %w", err)
	}

	if s.saveRepoCommitBlocks {
		s.logger.Info("saving block data for repo commits, this might take a lot of space")
	} else {
		s.logger.Info("not saving block data for repo commits")
	}

	// Keep track of the cursor occasionally.
	cur := lastCur
	first := true
	updateCursorFunc := func(seqNo int64) {
		if first {
			s.logger.Info("got first message", "seqNo", seqNo, "requestedSeqNo", lastCur, "resetCursor", s.resetCursor)
			if seqNo != lastCur && lastCur != 0 {
				s.logger.Warn("probably missed messages", "seqNo", seqNo, "requestedSeqNo", lastCur)
			}
			first = false
		}

		if seqNo%SEQUENCE_NUMBER_PERSISTENCE_INTERVAL == 0 {
			livenessChan <- struct{}{}

			if err := s.updateLastCursor(seqNo); err != nil {
				s.logger.Error("failed to persist cursor", "err", err)
			}
		}
	}

	rsc := &events.RepoStreamCallbacks{
		RepoCommit: func(evt *atproto.SyncSubscribeRepos_Commit) error {
			defer updateCursorFunc(evt.Seq)

			cur = evt.Seq
			logEvt := s.logger.With("repo", evt.Repo, "rev", evt.Rev, "seq", evt.Seq)
			logEvt.Debug("REPO_COMMIT", "ops", evt.Ops)

			// Remove blocks, save space.
			if !s.saveRepoCommitBlocks {
				evt.Blocks = nil
			}

			jsonEvent := event.JsonEvent{
				Received:   time.Now(),
				RepoCommit: evt,
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
		RepoIdentity: func(evt *atproto.SyncSubscribeRepos_Identity) error {
			defer updateCursorFunc(evt.Seq)

			cur = evt.Seq
			logEvt := s.logger.With("did", evt.Did, "seq", evt.Seq)
			logEvt.Debug("REPO_IDENTITY")

			jsonEvent := event.JsonEvent{
				Received:     time.Now(),
				RepoIdentity: evt,
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
		RepoAccount: func(evt *atproto.SyncSubscribeRepos_Account) error {
			defer updateCursorFunc(evt.Seq)

			cur = evt.Seq
			logEvt := s.logger.With("did", evt.Did, "seq", evt.Seq)
			logEvt.Debug("REPO_ACCOUNT", "active", evt.Active, "status", evt.Status, "time", evt.Time)

			jsonEvent := event.JsonEvent{
				Received:    time.Now(),
				RepoAccount: evt,
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
		RepoInfo: func(evt *atproto.SyncSubscribeRepos_Info) error {
			// TODO this does not have a sequence number, why?

			logEvt := s.logger.With("name", evt.Name)
			logEvt.Debug("REPO_INFO", "message", evt.Message)

			jsonEvent := event.JsonEvent{
				Received: time.Now(),
				RepoInfo: evt,
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
		LabelLabels: func(evt *atproto.LabelSubscribeLabels_Labels) error {
			defer updateCursorFunc(evt.Seq)
			// TODO this should never be received via the Firehose

			cur = evt.Seq
			logEvt := s.logger.With("seq", evt.Seq)
			logEvt.Warn("unexpected LABEL_LABELS", "labels", evt.Labels)

			jsonEvent := event.JsonEvent{
				Received:    time.Now(),
				LabelLabels: evt,
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
		LabelInfo: func(evt *atproto.LabelSubscribeLabels_Info) error {
			// TODO this also has no sequence number...
			// TODO this should never be received via the Firehose

			s.logger.Warn("unexpected LABEL_INFO", "name", evt.Name, "message", evt.Message)

			jsonEvent := event.JsonEvent{
				Received:  time.Now(),
				LabelInfo: evt,
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
		Error: func(evt *events.ErrorFrame) error {
			s.logger.Warn("ERROR_FRAME", "evt", evt)

			jsonEvent := event.JsonEvent{
				Received: time.Now(),
				ErrorFrame: &event.JsonErrorFrame{
					Error:   evt.Error,
					Message: evt.Message,
				},
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
	}

	err = events.HandleRepoStream(
		ctx, con, sequential.NewScheduler(
			"firehose-logger",
			rsc.EventHandler,
		),
		s.logger,
	)
	if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "closed network connection") {
		s.logger.Error("error listening for events", "err", err)
		s.closed <- err
	}

	// Save current sequence number
	if err := s.updateLastCursor(cur); err != nil {
		s.logger.Error("failed to persist cursor", "err", err)
	}

	// Shut down the writer
	err2 := s.writer.Stop()
	if err2 != nil {
		s.logger.Error("unable to shut down writer", "err", err)
		s.closed <- err2
	}

	return nil
}

func setupDB(dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
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

func setupSubscriber(logger *slog.Logger, baseOutputDir string,
	firehoseRemote string, postgresDsn string, numEventsPerFile int,
	saveBlocks bool, resetCursor bool) (*Subscriber, error) {
	db, err := setupDB(postgresDsn)
	if err != nil {
		return nil, err
	}

	logger.Info("running database migrations")
	err = db.AutoMigrate(&LastSeq{})
	if err != nil {
		return nil, err
	}

	u, err := url.Parse(firehoseRemote)
	if err != nil {
		return nil, err
	}

	writer, err := event.NewWriter(baseOutputDir, logger.With("component", "writer"), numEventsPerFile)
	if err != nil {
		return nil, err
	}

	s := &Subscriber{
		db:                   db,
		bgsUrl:               *u,
		logger:               logger.With("component", "subscriber"),
		writer:               writer,
		saveRepoCommitBlocks: saveBlocks,
		resetCursor:          resetCursor,
		closed:               make(chan error),
	}

	go writer.Run()

	return s, nil
}

func main() {
	var outputDir = flag.String("outdir", DEFAULT_OUTPUT_DIR, "Path to the base output directory")
	var postgresDsn = flag.String("db", DEFAULT_POSTGRES_DSN, "DSN to connect to a postgres database")
	var firehoseRemote = flag.String("firehose", DEFAULT_FIREHOSE_REMOTE, "Firehose to connect to")
	var resetCursor = flag.Bool("reset-cursor", false, "Reset stored cursor to current cursor reported by the remote")
	var numEntriesPerFile = flag.Uint("entries-per-file", DEFAULT_NUM_EVENTS_PER_FILE, "The number of events after which the output file is rotated")
	var debug = flag.Bool("debug", false, "Whether to enable debug logging")
	var saveBlocks = flag.Bool("save-blocks", false, "Whether to save binary block data for repo commits. This can take a lot of space.")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "%s [flags]\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Flags:")
		flag.PrintDefaults()
	}

	flag.Parse()

	if len(*outputDir) == 0 || len(*firehoseRemote) == 0 || len(*postgresDsn) == 0 || *numEntriesPerFile == 0 {
		flag.Usage()
		os.Exit(2)
	}

	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	subscriber, err := setupSubscriber(logger, *outputDir, *firehoseRemote, *postgresDsn, int(*numEntriesPerFile), *saveBlocks, *resetCursor)
	if err != nil {
		logger.Error("unable to set up subscriber", "err", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	livenessChan := make(chan struct{})
	deadShutdownChan := make(chan struct{})
	go func() {
		livenessTimeout := 5 * time.Minute
		t := time.NewTicker(livenessTimeout)
		for {
			select {
			case <-livenessChan:
				// We got something, reset the ticker
				t.Reset(livenessTimeout)
			case <-t.C:
				// Timer expired, shut down
				logger.Error("liveness check failed")
				close(deadShutdownChan)
				// Maybe drain this to be nice?
				for range livenessChan {
				}
				return
			}

		}
	}()

	go func() {
		err := subscriber.Run(ctx, livenessChan)
		if err != nil {
			// This only returns an error if we were unable to subscribe.
			// Later errors are passed via subscriber.closed, and handled.
			logger.Error("unable to set up subscriber", "err", err)
			os.Exit(1)
		}
	}()

	// Set up channel on which to send signal notifications.
	// We must use a buffered channel or risk missing the signal
	// if we're not ready to receive when the signal is sent.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	fmt.Println("Press Ctrl-C to stop")

	// Block until a signal is received or the subscriber dies.
	select {
	case <-c: // Ctrl-C, ok...
	case <-deadShutdownChan:
		logger.Error("liveness check failed, shutting down...")
	case err = <-subscriber.closed:
		logger.Error("subscriber died, shutting down", "err", err)
	}

	fmt.Println("Shutting down...")
	r := make(chan error)
	go func() {
		cancel()
		for err := range subscriber.closed {
			if err != nil {
				logger.Error("subscriber did not shut down cleanly", "err", err)
			}
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
			logger.Error("subscriber did not shut down cleanly", "err", err)
			os.Exit(1)
		}
	}

	// Close this for cleanup, why not.
	close(livenessChan)
}
