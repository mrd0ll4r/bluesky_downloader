package event

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"sync"
	"time"
)

// A RotatingWriter writes a sequence of JSON-encodable events to a file.
// The output file is rotated and compressed after a number of events.
// On clean shutdown using Stop, the last file is compressed.
type RotatingWriter struct {
	baseDir          string
	eventsIn         chan any
	logger           *slog.Logger
	numEventsPerFile int

	curFile      *os.File
	curEncoder   *json.Encoder
	curPath      string
	curNumEvents int

	// WaitGroup to track background compressions.
	wg   sync.WaitGroup
	stop chan struct{}
}

func NewWriter(baseDir string, logger *slog.Logger, numEventsPerFile int) (*RotatingWriter, error) {
	err := os.MkdirAll(baseDir, 0777)
	if err != nil {
		return nil, fmt.Errorf("unable to mkdir: %w", err)
	}

	w := &RotatingWriter{
		baseDir:          baseDir,
		eventsIn:         make(chan any),
		logger:           logger,
		numEventsPerFile: numEventsPerFile,
		stop:             make(chan struct{}),
	}

	return w, nil
}

func (w *RotatingWriter) WriteEvent(e any) {
	w.eventsIn <- e
}

func (w *RotatingWriter) Stop() error {
	w.logger.Info("stopping writer")
	close(w.eventsIn)

	<-w.stop
	w.logger.Info("writer stopped, waiting for outstanding background compressions...")
	w.wg.Wait()

	w.logger.Info("compressing last written file...")
	if w.curFile != nil {
		return w.backgroundCompress(w.curFile, w.curPath)
	}

	return nil
}

func (w *RotatingWriter) backgroundCompress(f *os.File, originalPath string) (err error) {
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

func (w *RotatingWriter) createFile() (*os.File, string, error) {
	ts := time.Now().Unix()
	p := path.Join(w.baseDir, fmt.Sprintf("%d.json", ts))

	// Make sure this is actually a different path than the currently-open file.
	// This collision can happen when we have a _lot_ of events, which happens
	// if we catch up.
	if p == w.curPath {
		// Crude, but this should work.
		w.logger.Warn("sleeping a second to avoid output file name collision", "path", p)
		time.Sleep(1 * time.Second)
		return w.createFile()
	}

	f, err := os.Create(p)
	return f, p, err
}

func (w *RotatingWriter) maybeRotateFile() error {
	if w.curNumEvents >= w.numEventsPerFile {
		// Rotate, avoid data race
		curFile := w.curFile
		curPath := w.curPath
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			err := w.backgroundCompress(curFile, curPath)
			if err != nil {
				w.logger.Error("unable to compress file", "err", err)
			}
		}()
	}
	if w.curFile == nil || w.curNumEvents >= w.numEventsPerFile {
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

func (w *RotatingWriter) Run() {
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
