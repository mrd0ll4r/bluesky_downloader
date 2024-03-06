package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/events"
	"github.com/bluesky-social/indigo/events/schedulers/sequential"
	"github.com/bluesky-social/indigo/xrpc"
	"github.com/carlmjohnson/versioninfo"
	"github.com/gorilla/websocket"
	"github.com/labstack/gommon/log"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"time"
)

var (
	USER_AGENT          = "github.com/mrd0ll4r/bluesky_downloader/firehose_logger"
	OUTPUT_DIR          = "firehose_logs"
	NUM_EVENTS_PER_FILE = 10_000
	POSTGRES_DSN        = "postgres://bluesky_indexer:bluesky_indexer@localhost:5434/bluesky_indexer"
)

type Writer struct {
	baseDir          string
	eventsIn         chan jsonEvent
	logger           *slog.Logger
	numEventsPerFile int

	curFile      *os.File
	curEncoder   *json.Encoder
	curPath      string
	curNumEvents int

	stop chan struct{}
}

func NewWriter(baseDir string, logger *slog.Logger, numEventsPerFile int) (*Writer, error) {
	err := os.MkdirAll(baseDir, 0777)
	if err != nil {
		return nil, fmt.Errorf("unable to mkdir: %w", err)
	}

	w := &Writer{
		baseDir:          baseDir,
		eventsIn:         make(chan jsonEvent),
		logger:           logger,
		numEventsPerFile: numEventsPerFile,
		stop:             make(chan struct{}),
	}

	return w, nil
}

func (w *Writer) Stop() error {
	w.logger.Info("stopping writer")
	close(w.eventsIn)

	<-w.stop
	w.logger.Info("writer stopped, compressing last written file...")

	if w.curFile != nil {
		return w.backgroundCompress(w.curFile, w.curPath)
	}

	return nil
}

func (w *Writer) backgroundCompress(f *os.File, originalPath string) (err error) {
	gzipPath := fmt.Sprintf("%s.gz", originalPath)

	defer func() {
		if err != nil {
			w.logger.Warn("background compression failed, not removing source file, attempting to remove potentially-incomplete gzipped file", "originalPath", originalPath)
			e := os.Remove(gzipPath)
			if e != nil {
				w.logger.Warn("unable to remove gzipped file", "err", e)
			}
		} else {
			w.logger.Info("background compression complete, removing source file", "originalPath", originalPath)
			e := os.Remove(originalPath)
			if e != nil {
				w.logger.Warn("unable to remove original file", "err", e)
			}
		}
	}()
	defer f.Close()

	gzipF, err := os.Create(gzipPath)
	if err != nil {
		return fmt.Errorf("unable to create gzip file: %w", err)
	}
	defer gzipF.Close()

	gzipW, err := gzip.NewWriterLevel(gzipF, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("unable to create gzip writer: %w", err)
	}
	defer gzipW.Close()

	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("unable to seek: %w", err)
	}

	_, err = io.Copy(gzipW, f)
	if err != nil {
		return fmt.Errorf("unable to copy: %w", err)
	}

	return nil
}

func (w *Writer) createFile() (*os.File, string, error) {
	ts := time.Now().Unix()
	p := path.Join(w.baseDir, fmt.Sprintf("%d.json", ts))

	f, err := os.Create(p)
	return f, p, err
}

func (w *Writer) maybeRotateFile() error {
	if w.curNumEvents >= NUM_EVENTS_PER_FILE {
		// Rotate, avoid data rate
		curFile := w.curFile
		curPath := w.curPath
		go func() {
			err := w.backgroundCompress(curFile, curPath)
			if err != nil {
				w.logger.Error("unable to compress file", "err", err)
			}
		}()
	}
	if w.curFile == nil || w.curNumEvents >= NUM_EVENTS_PER_FILE {
		// Create new file
		f, p, err := w.createFile()
		if err != nil {
			return err
		}
		w.curFile = f
		w.curEncoder = json.NewEncoder(f)
		w.curPath = p
		w.curNumEvents = 0
	}

	return nil
}

func (w *Writer) Run() {
	defer func() {
		w.logger.Info("writer loop shut down")
		close(w.stop)
	}()
	for job := range w.eventsIn {
		err := w.maybeRotateFile()
		if err != nil {
			// TODO what now?
			panic(err)
		}
		err = w.curEncoder.Encode(job)
		if err != nil {
			w.logger.Error("unable to encode", "job", job)
		}
		w.curNumEvents++
	}
	w.logger.Info("shutting down writer loop")
}

type Server struct {
	db      *gorm.DB
	bgsxrpc *xrpc.Client
	logger  *slog.Logger
	writer  *Writer
	closed  chan error
}

type LastSeq struct {
	ID  uint `gorm:"primarykey"`
	Seq int64
}

func (s *Server) getLastCursor() (int64, error) {
	var lastSeq LastSeq
	if err := s.db.Find(&lastSeq).Error; err != nil {
		return 0, err
	}

	if lastSeq.ID == 0 {
		return 0, s.db.Create(&lastSeq).Error
	}

	return lastSeq.Seq, nil
}

func (s *Server) updateLastCursor(curs int64) error {
	return s.db.Model(LastSeq{}).Where("id = 1").Update("seq", curs).Error
}

func (s *Server) Run(ctx context.Context) error {
	defer func() { close(s.closed) }()
	cur, err := s.getLastCursor()
	if err != nil {
		return fmt.Errorf("get last cursor: %w", err)
	}

	d := websocket.DefaultDialer
	u, err := url.Parse("wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos")
	if err != nil {
		return fmt.Errorf("invalid bgshost URI: %w", err)
	}
	if cur != 0 {
		u.RawQuery = fmt.Sprintf("cursor=%d", cur)
	}
	con, _, err := d.Dial(u.String(), http.Header{
		"User-Agent": []string{fmt.Sprintf("firehose-logger/%s", versioninfo.Short())},
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

			jsonEvent := jsonEvent{
				Received:   time.Now(),
				RepoCommit: evt,
			}
			s.writer.eventsIn <- jsonEvent

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

			jsonEvent := jsonEvent{
				Received:   time.Now(),
				RepoHandle: evt,
			}
			s.writer.eventsIn <- jsonEvent

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

			jsonEvent := jsonEvent{
				Received:     time.Now(),
				RepoIdentity: evt,
			}
			s.writer.eventsIn <- jsonEvent

			return nil
		},
		RepoInfo: func(evt *atproto.SyncSubscribeRepos_Info) error {
			// TODO this does not have a sequence number, why?

			logEvt := s.logger.With("name", evt.Name)
			logEvt.Debug("REPO_INFO", "message", evt.Message)

			jsonEvent := jsonEvent{
				Received: time.Now(),
				RepoInfo: evt,
			}
			s.writer.eventsIn <- jsonEvent

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

			jsonEvent := jsonEvent{
				Received:    time.Now(),
				RepoMigrate: evt,
			}
			s.writer.eventsIn <- jsonEvent

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

			jsonEvent := jsonEvent{
				Received:      time.Now(),
				RepoTombstone: evt,
			}
			s.writer.eventsIn <- jsonEvent

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

			jsonEvent := jsonEvent{
				Received:    time.Now(),
				LabelLabels: evt,
			}
			s.writer.eventsIn <- jsonEvent

			return nil
		},
		LabelInfo: func(evt *atproto.LabelSubscribeLabels_Info) error {
			// TODO this also has no sequence number...

			s.logger.Debug("LABEL_INFO", "name", evt.Name, "message", evt.Message)

			jsonEvent := jsonEvent{
				Received:  time.Now(),
				LabelInfo: evt,
			}
			s.writer.eventsIn <- jsonEvent

			return nil
		},
		Error: func(evt *events.ErrorFrame) error {
			s.logger.Info("ERROR_FRAME", "evt", evt)

			jsonEvent := jsonEvent{
				Received: time.Now(),
				ErrorFrame: &JsonErrorFrame{
					Error:   evt.Error,
					Message: evt.Message,
				},
			}
			s.writer.eventsIn <- jsonEvent

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

type JsonErrorFrame struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

type jsonEvent struct {
	Received      time.Time                             `json:"received"`
	RepoCommit    *atproto.SyncSubscribeRepos_Commit    `json:"repo_commit"`
	RepoHandle    *atproto.SyncSubscribeRepos_Handle    `json:"repo_handle"`
	RepoIdentity  *atproto.SyncSubscribeRepos_Identity  `json:"repo_identity"`
	RepoInfo      *atproto.SyncSubscribeRepos_Info      `json:"repo_info"`
	RepoMigrate   *atproto.SyncSubscribeRepos_Migrate   `json:"repo_migrate"`
	RepoTombstone *atproto.SyncSubscribeRepos_Tombstone `json:"repo_tombstone"`
	LabelLabels   *atproto.LabelSubscribeLabels_Labels  `json:"label_labels"`
	LabelInfo     *atproto.LabelSubscribeLabels_Info    `json:"label_info"`
	ErrorFrame    *JsonErrorFrame                       `json:"error_frame"`
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
	err = db.AutoMigrate(&LastSeq{})
	if err != nil {
		return nil, err
	}

	bgshttp := "https://bsky.network"
	bgsxrpc := &xrpc.Client{
		Host: bgshttp,
	}
	bgsxrpc.UserAgent = &USER_AGENT

	writer, err := NewWriter(OUTPUT_DIR, logger.With("component", "writer"), NUM_EVENTS_PER_FILE)
	if err != nil {
		return nil, err
	}

	s := &Server{
		db:      db,
		bgsxrpc: bgsxrpc,
		logger:  logger.With("component", "server"),
		writer:  writer,
		closed:  make(chan error),
	}

	go writer.Run()

	return s, nil
}

func main() {
	server, err := doStuff()
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		err := server.Run(ctx)
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
		cancel()
		for err := range server.closed {
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
