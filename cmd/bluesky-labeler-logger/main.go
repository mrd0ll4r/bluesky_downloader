package main

import (
	"bluesky-downloader/event"
	"context"
	"errors"
	"fmt"
	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/bluesky-social/indigo/events"
	"github.com/bluesky-social/indigo/events/schedulers/sequential"
	"github.com/gorilla/websocket"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"time"

	flag "github.com/spf13/pflag"
)

var (
	USER_AGENT                  = "github.com/mrd0ll4r/bluesky_downloader/labeler_logger"
	DEFAULT_OUTPUT_DIR          = "./labeler_logs"
	DEFAULT_NUM_EVENTS_PER_FILE = uint(1_000)
	DEFAULT_POSTGRES_DSN        = "postgres://bluesky_indexer:bluesky_indexer@localhost:5434/bluesky_indexer"
)

type Subscriber struct {
	db         *gorm.DB
	labelerDid syntax.DID
	labelerURI url.URL
	logger     *slog.Logger
	writer     *event.RotatingWriter
	closed     chan error
}

type LabelerLastSeq struct {
	DID string `gorm:"primarykey;column:did"`
	Seq int64
}

func (s *Subscriber) getLastCursor() (int64, error) {
	var lastSeq LabelerLastSeq
	if err := s.db.Find(&lastSeq, "did = ?", s.labelerDid.String()).Error; err != nil {
		return 0, err
	}

	if lastSeq.DID == "" {
		lastSeq.DID = s.labelerDid.String()
		return 0, s.db.Create(&lastSeq).Error
	}

	return lastSeq.Seq, nil
}

func (s *Subscriber) updateLastCursor(curs int64) error {
	return s.db.Model(LabelerLastSeq{}).Where("did = ?", s.labelerDid.String()).Update("seq", curs).Error
}

func (s *Subscriber) Run(ctx context.Context) error {
	defer func() { close(s.closed) }()
	cur, err := s.getLastCursor()
	if err != nil {
		return fmt.Errorf("get last cursor: %w", err)
	}
	if cur == 0 {
		// We might as well try to get all the labels,
		// hopefully they still serve them.
		// TODO this could be improved: We could try cursor=1 and see what we
		// get: If it actually starts at 1 (or 2?): good. If not, take whatever
		// it returns and subtract some number, maybe 10k? Unclear how much
		// buffering is going on...
		s.logger.Info("have no cursor (or never received a label), will attempt to start at 1...")
	}

	d := websocket.DefaultDialer
	u := s.labelerURI
	u.Scheme = "wss"
	u.Path = "/xrpc/com.atproto.label.subscribeLabels"
	// We always provide the cursor. In particular, if it's zero, we should get
	// all past labels.
	u.RawQuery = fmt.Sprintf("cursor=%d", cur)
	s.logger.Info("attempting to subscribe", "url", u.String())
	con, _, err := d.Dial(u.String(), http.Header{
		"User-Agent": []string{USER_AGENT},
	})
	if err != nil {
		return fmt.Errorf("events dial failed: %w", err)
	}
	s.logger.Debug("dial successful, setting up subscription...", "url", u.String())

	rsc := &events.RepoStreamCallbacks{
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
			// TODO this has no sequence number...

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
			"labeler-logger",
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

var ErrMissingLabelerServiceEndpoint = errors.New("missing atproto_labeler service entry")

func setupSubscriber(logger *slog.Logger, did syntax.DID, baseOutputDir string, pgDsn string, entriesPerFile int) (*Subscriber, error) {
	db, err := setupDB(pgDsn)
	if err != nil {
		return nil, err
	}

	logger.Info("running database migrations")
	err = db.AutoMigrate(&LabelerLastSeq{})
	if err != nil {
		return nil, err
	}

	// Set up DID directory
	dir := identity.BaseDirectory{
		PLCURL: identity.DefaultPLCURL,
		HTTPClient: http.Client{
			Timeout: time.Second * 15,
		},
		Resolver: net.Resolver{
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: time.Second * 5}
				return d.DialContext(ctx, network, address)
			},
		},
		TryAuthoritativeDNS: true,
		// primary Bluesky PDS instance only supports HTTP resolution method
		SkipDNSDomainSuffixes: []string{".bsky.social"},
	}

	// Resolve the DID to a labeler service endpoint
	ident, err := dir.LookupDID(context.Background(), did)
	if err != nil {
		return nil, fmt.Errorf("unable to look up DID: %w", err)
	}
	labelerEndpoint := ident.GetServiceEndpoint("atproto_labeler")
	if labelerEndpoint == "" {
		return nil, ErrMissingLabelerServiceEndpoint
	}
	u, err := url.Parse(labelerEndpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid labeler service endpoint URL: %w", err)
	}

	sanitizedDid := strings.ReplaceAll(ident.DID.String(), ":", "_")
	outPath := path.Join(baseOutputDir, sanitizedDid)

	writer, err := event.NewWriter(outPath, logger.With("component", "writer"), entriesPerFile)
	if err != nil {
		return nil, err
	}

	s := &Subscriber{
		db:         db,
		labelerDid: did,
		labelerURI: *u,
		logger:     logger.With("component", "server"),
		writer:     writer,
		closed:     make(chan error),
	}

	go writer.Run()

	return s, nil
}

func main() {
	var outputDir = flag.String("outdir", DEFAULT_OUTPUT_DIR, "Path to the base output directory")
	var postgresDsn = flag.String("db", DEFAULT_POSTGRES_DSN, "DSN to connect to a postgres database")
	var numEntriesPerFile = flag.Uint("entries-per-file", DEFAULT_NUM_EVENTS_PER_FILE, "The number of events after which the output file is rotated")
	var debug = flag.Bool("debug", false, "Whether to enable debug logging")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "%s [flags] <DID>\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Arguments:")
		fmt.Fprintf(os.Stderr, "      <DID>\t\tDID of the labeler to subscribe to\n")
		fmt.Fprintln(os.Stderr, "Flags:")
		flag.PrintDefaults()
	}

	flag.Parse()

	if len(*outputDir) == 0 || len(*postgresDsn) == 0 || *numEntriesPerFile == 0 || flag.NArg() != 1 {
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

	d, err := syntax.ParseDID(flag.Arg(0))
	if err != nil {
		logger.Error("unable to parse DID", "err", err)
		// IMPORTANT: Exit code 3 to signal systemd not to attempt a restart.
		os.Exit(3)
	}
	logger = logger.With("labeler", d.String())
	slog.SetDefault(logger)

	subscriber, err := setupSubscriber(logger, d, *outputDir, *postgresDsn, int(*numEntriesPerFile))
	if err != nil {
		logger.Error("unable to set up subscriber", "err", err)
		if errors.Is(err, ErrMissingLabelerServiceEndpoint) {
			// IMPORTANT: Exit code 3 to signal systemd not to attempt a restart.
			os.Exit(3)
		}
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		err := subscriber.Run(ctx)
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
	errExit := false
	select {
	case <-c: // Ctrl-C, ok...
	case err = <-subscriber.closed:
		logger.Error("subscriber died, shutting down", "err", err)
		errExit = true
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

	if errExit {
		os.Exit(1)
	}
}
