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
	"github.com/labstack/gommon/log"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"
)

var (
	USER_AGENT          = "github.com/mrd0ll4r/bluesky_downloader/firehose_logger"
	OUTPUT_DIR          = "firehose_logs"
	NUM_EVENTS_PER_FILE = 10_000
	POSTGRES_DSN        = "postgres://bluesky_indexer:bluesky_indexer@localhost:5434/bluesky_indexer"
)

type Subscriber struct {
	db     *gorm.DB
	bgsUrl url.URL
	logger *slog.Logger
	writer *event.Writer
	closed chan error
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
	return s.db.Model(LastSeq{}).Where("id = 1").Update("seq", curs).Error
}

func (s *Subscriber) Run(ctx context.Context) error {
	defer func() { close(s.closed) }()
	cur, err := s.getLastCursor()
	if err != nil {
		return fmt.Errorf("get last cursor: %w", err)
	}

	d := websocket.DefaultDialer
	u := s.bgsUrl
	u.Scheme = "wss"
	u.Path = "/xrpc/com.atproto.sync.subscribeRepos"
	if cur != 0 {
		u.RawQuery = fmt.Sprintf("cursor=%d", cur)
	}
	con, _, err := d.Dial(u.String(), http.Header{
		"User-Agent": []string{USER_AGENT},
	})
	if err != nil {
		return fmt.Errorf("events dial failed: %w", err)
	}

	rsc := &events.RepoStreamCallbacks{
		RepoCommit: func(evt *atproto.SyncSubscribeRepos_Commit) error {
			defer func() {
				if evt.Seq%50 == 0 {
					if err := s.updateLastCursor(evt.Seq); err != nil {
						s.logger.Error("failed to persist cursor", "err", err)
					}
				}
			}()
			cur = evt.Seq
			logEvt := s.logger.With("repo", evt.Repo, "rev", evt.Rev, "seq", evt.Seq)
			logEvt.Debug("REPO_COMMIT", "ops", evt.Ops)

			jsonEvent := event.JsonEvent{
				Received:   time.Now(),
				RepoCommit: evt,
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
		RepoHandle: func(evt *atproto.SyncSubscribeRepos_Handle) error {
			defer func() {
				if evt.Seq%50 == 0 {
					if err := s.updateLastCursor(evt.Seq); err != nil {
						s.logger.Error("failed to persist cursor", "err", err)
					}
				}
			}()
			cur = evt.Seq
			logEvt := s.logger.With("did", evt.Did, "seq", evt.Seq)
			logEvt.Debug("REPO_HANDLE", "handle", evt.Handle)

			jsonEvent := event.JsonEvent{
				Received:   time.Now(),
				RepoHandle: evt,
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
		RepoIdentity: func(evt *atproto.SyncSubscribeRepos_Identity) error {
			defer func() {
				if evt.Seq%50 == 0 {
					if err := s.updateLastCursor(evt.Seq); err != nil {
						s.logger.Error("failed to persist cursor", "err", err)
					}
				}
			}()
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
		RepoMigrate: func(evt *atproto.SyncSubscribeRepos_Migrate) error {
			defer func() {
				if evt.Seq%50 == 0 {
					if err := s.updateLastCursor(evt.Seq); err != nil {
						s.logger.Error("failed to persist cursor", "err", err)
					}
				}
			}()
			cur = evt.Seq
			logEvt := s.logger.With("did", evt.Did, "seq", evt.Seq)
			logEvt.Debug("REPO_MIGRATE", "migrate_to", evt.MigrateTo)

			jsonEvent := event.JsonEvent{
				Received:    time.Now(),
				RepoMigrate: evt,
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
		RepoTombstone: func(evt *atproto.SyncSubscribeRepos_Tombstone) error {
			defer func() {
				if evt.Seq%50 == 0 {
					if err := s.updateLastCursor(evt.Seq); err != nil {
						s.logger.Error("failed to persist cursor", "err", err)
					}
				}
			}()
			cur = evt.Seq
			logEvt := s.logger.With("did", evt.Did, "seq", evt.Seq)
			logEvt.Debug("REPO_TOMBSTONE")

			jsonEvent := event.JsonEvent{
				Received:      time.Now(),
				RepoTombstone: evt,
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
		LabelLabels: func(evt *atproto.LabelSubscribeLabels_Labels) error {
			defer func() {
				if evt.Seq%50 == 0 {
					if err := s.updateLastCursor(evt.Seq); err != nil {
						s.logger.Error("failed to persist cursor", "err", err)
					}
				}
			}()
			cur = evt.Seq
			logEvt := s.logger.With("seq", evt.Seq)
			logEvt.Debug("LABEL_LABELS", "labels", evt.Labels)

			jsonEvent := event.JsonEvent{
				Received:    time.Now(),
				LabelLabels: evt,
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
		LabelInfo: func(evt *atproto.LabelSubscribeLabels_Info) error {
			// TODO this also has no sequence number...

			s.logger.Debug("LABEL_INFO", "name", evt.Name, "message", evt.Message)

			jsonEvent := event.JsonEvent{
				Received:  time.Now(),
				LabelInfo: evt,
			}
			s.writer.WriteEvent(jsonEvent)

			return nil
		},
		Error: func(evt *events.ErrorFrame) error {
			s.logger.Info("ERROR_FRAME", "evt", evt)

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

func setupSubscriber() (*Subscriber, error) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	db, err := setupDB()
	if err != nil {
		return nil, err
	}

	logger.Info("running database migrations")
	err = db.AutoMigrate(&LastSeq{})
	if err != nil {
		return nil, err
	}

	u, err := url.Parse("https://bsky.network")
	if err != nil {
		return nil, err
	}

	writer, err := event.NewWriter(OUTPUT_DIR, logger.With("component", "writer"), NUM_EVENTS_PER_FILE)
	if err != nil {
		return nil, err
	}

	s := &Subscriber{
		db:     db,
		bgsUrl: *u,
		logger: logger.With("component", "subscriber"),
		writer: writer,
		closed: make(chan error),
	}

	go writer.Run()

	return s, nil
}

func main() {
	subscriber, err := setupSubscriber()
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		err := subscriber.Run(ctx)
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

	// Block until a signal is received or the subscriber dies.
	select {
	case <-c: // Ctrl-C, ok...
	case err = <-subscriber.closed:
		log.Error("subscriber died, shutting down", "err", err)
	}

	fmt.Println("Shutting down...")
	r := make(chan error)
	go func() {
		cancel()
		for err := range subscriber.closed {
			if err != nil {
				log.Error(err)
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
			panic(err)
		}
	}
}
