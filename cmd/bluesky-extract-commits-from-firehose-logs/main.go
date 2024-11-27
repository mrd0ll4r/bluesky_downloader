package main

import (
	"bluesky-downloader/event"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/data"
	"github.com/bluesky-social/indigo/repo"
	"github.com/bluesky-social/indigo/repomgr"
	flag "github.com/spf13/pflag"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Event is a Firehose-received event, parsed and translated to JSON for further
// processing.
type Event struct {
	Did      string                                  `json:"did"`
	Received string                                  `json:"received"`
	Time     string                                  `json:"time"`
	Seq      int64                                   `json:"seq"`
	Commit   *Commit                                 `json:"commit,omitempty"`
	Account  *comatproto.SyncSubscribeRepos_Account  `json:"account,omitempty"`
	Identity *comatproto.SyncSubscribeRepos_Identity `json:"identity,omitempty"`
}

// Commit is a repo commit.
type Commit struct {
	Rev         string     `json:"rev"`
	Since       *string    `json:"since"`
	CID         string     `json:"cid"`
	Ops         []CommitOp `json:"ops"`
	DecodeError *string    `json:"decode_error,omitempty"`
	Blocks      []byte     `json:"blocks,omitempty"`
	TooBig      bool       `json:"too_big,omitempty"`
	Blobs       []string   `json:"blobs,omitempty"`
}

// CommitOp is one operation in a repo commit.
type CommitOp struct {
	Operation   repomgr.EventKind `json:"operation"`
	Collection  string            `json:"collection"`
	RKey        string            `json:"rkey"`
	Record      json.RawMessage   `json:"record,omitempty"`
	CID         string            `json:"cid,omitempty"`
	DecodeError *string           `json:"decode_error,omitempty"`
}

func extractCommitOpInner(ctx context.Context, log *slog.Logger, op *comatproto.SyncSubscribeRepos_RepoOp, repo *repo.Repo) (*CommitOp, error) {
	collection := strings.Split(op.Path, "/")[0]
	rkey := strings.Split(op.Path, "/")[1]
	ek := repomgr.EventKind(op.Action)
	log = log.With("collection", collection, "rkey", rkey, "op", ek)

	switch ek {
	case repomgr.EvtKindCreateRecord:
		if op.Cid == nil {
			log.Error("update record op missing cid")
			return nil, fmt.Errorf("update record op missing cid")
		}

		rcid, recB, err := repo.GetRecordBytes(ctx, op.Path)
		if err != nil {
			log.Error("failed to get record bytes", "error", err)
			return nil, fmt.Errorf("failed to get record bytes: %w", err)
		}
		// TODO sometimes we fail to find a record because the MST returns a
		// different CID than the one encoded in the operation. Why is this?
		// Should we try the operation CID as well, or is this a malformed repo?

		log.Debug("checking CID mismatch", "op.Cid", op.Cid.String(), "recCid", rcid.String())

		recCid := rcid.String()
		if recCid != op.Cid.String() {
			log.Error("record cid mismatch", "expected", *op.Cid, "actual", rcid)
			return nil, fmt.Errorf("record cid mismatch, expected %s, got %s", *op.Cid, rcid)
		}

		rec, err := data.UnmarshalCBOR(*recB)
		if err != nil {
			log.Error("failed to unmarshal record", "error", err)
			return nil, fmt.Errorf("failed to unmarshal record: %w", err)
		}

		recJSON, err := json.Marshal(rec)
		if err != nil {
			log.Error("failed to marshal record to json", "error", err)
			return nil, fmt.Errorf("failed to marshal record to json: %w", err)
		}

		return &CommitOp{
			Operation:  ek,
			Collection: collection,
			RKey:       rkey,
			Record:     recJSON,
			CID:        recCid,
		}, nil
	case repomgr.EvtKindUpdateRecord:
		if op.Cid == nil {
			log.Error("update record op missing cid")
			return nil, fmt.Errorf("update record op missing cid")
		}

		rcid, recB, err := repo.GetRecordBytes(ctx, op.Path)
		if err != nil {
			log.Error("failed to get record bytes", "error", err)
			return nil, fmt.Errorf("failed to get record bytes: %w", err)
		}

		recCid := rcid.String()
		if recCid != op.Cid.String() {
			log.Error("record cid mismatch", "expected", *op.Cid, "actual", rcid)
			return nil, fmt.Errorf("record cid mismatch, expected %s, got %s", *op.Cid, rcid)
		}

		rec, err := data.UnmarshalCBOR(*recB)
		if err != nil {
			log.Error("failed to unmarshal record", "error", err)
			return nil, fmt.Errorf("failed to unmarshal record: %w", err)
		}

		recJSON, err := json.Marshal(rec)
		if err != nil {
			log.Error("failed to marshal record to json", "error", err)
			return nil, fmt.Errorf("failed to marshal record to json: %w", err)
		}

		return &CommitOp{
			Operation:  ek,
			Collection: collection,
			RKey:       rkey,
			Record:     recJSON,
			CID:        recCid,
		}, nil
	case repomgr.EvtKindDeleteRecord:
		o := CommitOp{
			Operation:  ek,
			Collection: collection,
			RKey:       rkey,
		}
		if op.Cid != nil {
			o.CID = op.Cid.String()
		}
		return &o, nil
	default:
		log.Error("unknown event kind from op action", "kind", op.Action)
		return nil, fmt.Errorf("unknown event kind from op action: %s", op.Action)
	}
}

func extractCommitOps(ctx context.Context, log *slog.Logger, commit *comatproto.SyncSubscribeRepos_Commit, repo *repo.Repo) ([]CommitOp, bool) {
	var toReturn []CommitOp
	anyError := false

	for _, op := range commit.Ops {
		collection := strings.Split(op.Path, "/")[0]
		rkey := strings.Split(op.Path, "/")[1]
		ek := repomgr.EventKind(op.Action)

		translated, err := extractCommitOpInner(ctx, log, op, repo)
		if err != nil {
			anyError = true
			tmp := CommitOp{
				Operation:   ek,
				Collection:  collection,
				RKey:        rkey,
				DecodeError: new(string),
			}
			*tmp.DecodeError = err.Error()
			toReturn = append(toReturn, tmp)
		} else {
			toReturn = append(toReturn, *translated)
		}
	}

	return toReturn, anyError
}

func handleRepoCommit(log *slog.Logger, commit *comatproto.SyncSubscribeRepos_Commit) Commit {
	ctx := context.Background()
	toReturn := Commit{
		Rev:    commit.Rev,
		Since:  commit.Since,
		CID:    commit.Commit.String(),
		TooBig: commit.TooBig,
	}
	log = log.With("did", commit.Repo, "cid", commit.Commit.String())

	// If this contains blobs, which I think never happens, include the blob
	// links and block data (maybe the blobs are in there, who knows).
	if len(commit.Blobs) != 0 {
		log.Warn("commit contains blobs, including block data for future decoding", "blobs", commit.Blobs)
		for _, blob := range commit.Blobs {
			toReturn.Blobs = append(toReturn.Blobs, blob.String())
		}
		toReturn.Blocks = commit.Blocks
	}

	// This is mostly taken from https://github.com/bluesky-social/jetstream/blob/main/pkg/consumer/consumer.go
	rr, err := repo.ReadRepoFromCar(ctx, bytes.NewReader(commit.Blocks))
	if err != nil {
		log.Error("failed to read repo from CAR", "error", err)
		toReturn.DecodeError = new(string)
		*toReturn.DecodeError = fmt.Errorf("failed to read repo from CAR: %w", err).Error()
		toReturn.Blocks = commit.Blocks
		return toReturn
	}

	// TODO check signature?
	//rr.SignedCommit()

	ops, anyErr := extractCommitOps(ctx, log, commit, rr)
	if anyErr {
		toReturn.Blocks = commit.Blocks
		toReturn.DecodeError = new(string)
		*toReturn.DecodeError = "unable to decode all operations"
	}
	toReturn.Ops = ops

	return toReturn
}

func main() {
	var debug = flag.Bool("debug", false, "Whether to enable debug logging")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "%s [flags]\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Flags:")
		flag.PrintDefaults()
	}

	flag.Parse()

	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)
	if *debug {
		logger.Debug("debug logging")
	}

	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)

	eventIn := event.JsonEvent{}
	for {
		err := decoder.Decode(&eventIn)
		if err != nil {
			if err == io.EOF {
				break
			}
			logger.Error("failed to decode json event", "error", err)
			os.Exit(1)
		}
		logger.Debug("decoded event", "event", eventIn)

		commit := eventIn.RepoCommit
		//account := eventIn.RepoAccount
		//identity := eventIn.RepoIdentity
		if commit != nil {
			translated := handleRepoCommit(logger, commit)
			logger.Debug("translated commit", "commit", translated)

			e := Event{
				Did:      commit.Repo,
				Received: eventIn.Received.Format(time.RFC3339),
				Time:     commit.Time,
				Seq:      commit.Seq,
				Commit:   &translated,
			}
			err := encoder.Encode(e)
			if err != nil {
				panic(err)
			}
		}
		/* else if account != nil {
			e := Event{
				Did:     account.Did,
				Time:    account.Time,
				Seq:     account.Seq,
				Account: account,
			}
			err := encoder.Encode(e)
			if err != nil {
				panic(err)
			}
		} else if identity != nil {
			e := Event{
				Did:      identity.Did,
				Time:     identity.Time,
				Seq:      identity.Seq,
				Identity: identity,
			}
			err := encoder.Encode(e)
			if err != nil {
				panic(err)
			}
		}*/
	}

}
