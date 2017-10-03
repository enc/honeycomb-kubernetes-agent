package tailer

import (
	"path/filepath"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/honeycombio/honeycomb-kubernetes-agent/handlers"
	"github.com/hpcloud/tail"
)

type Tailer struct {
	path          string
	done          chan bool
	handler       handlers.LineHandler
	stateRecorder StateRecorder
}

func NewTailer(path string, handler handlers.LineHandler, stateRecorder StateRecorder) *Tailer {
	t := &Tailer{
		path:          path,
		handler:       handler,
		stateRecorder: stateRecorder,
		done:          make(chan bool),
	}
	return t
}

func (t *Tailer) Run() error {
	seekInfo := &tail.SeekInfo{}
	if t.stateRecorder != nil {
		if offset, err := t.stateRecorder.Get(t.path); err == nil {
			seekInfo.Offset = offset
		}
	}
	tailConf := tail.Config{
		ReOpen: true,
		Follow: true,
		// TODO: inotify doesn't detect file deletions, fix this
		Poll:     true,
		Logger:   tail.DiscardingLogger,
		Location: seekInfo,
	}
	tailer, err := tail.TailFile(t.path, tailConf)
	if err != nil {
		logrus.WithField("filePath", t.path).Info("Error starting file tail")
		return err
	}
	logrus.WithField("path", t.path).WithField("offset", tailConf.Location.Offset).
		Info("Tailing file")
	ticker := time.NewTicker(time.Second)
	go func() {
	loop:
		for {
			select {
			case line, ok := <-tailer.Lines:
				if !ok {
					t.Clear()
					break loop
				}
				if line.Err != nil {
					continue
				}
				t.handler.Handle(line.Text)
			case <-t.done:
				ticker.Stop()
				break loop
			case <-ticker.C:
				if offset, err := tailer.Tell(); err == nil {
					t.updateState(offset)
				}
			}
		}
		if offset, err := tailer.Tell(); err == nil {
			t.updateState(offset)
		}
		logrus.WithField("filePath", t.path).Info("Done tailing file")
	}()
	return nil
}

func (t *Tailer) updateState(offset int64) {
	if t.stateRecorder != nil {
		t.stateRecorder.Record(t.path, offset)
	}
}

func (t *Tailer) Stop() {
	t.done <- true
}

func (t *Tailer) Clear() {
	if t.stateRecorder != nil {
		t.stateRecorder.Delete(t.path)
	}
}

type filterFunc func(string) bool

type PathWatcher struct {
	pattern        string
	filter         filterFunc
	tailers        map[string]*Tailer
	handlerFactory handlers.LineHandlerFactory
	stateRecorder  StateRecorder
	checkInterval  time.Duration
	done           chan bool
}

func NewPathWatcher(
	pattern string,
	filter filterFunc,
	handlerFactory handlers.LineHandlerFactory,
	stateRecorder StateRecorder,
) *PathWatcher {
	p := &PathWatcher{
		pattern:        pattern,
		filter:         filter,
		tailers:        make(map[string]*Tailer),
		handlerFactory: handlerFactory,
		stateRecorder:  stateRecorder,
		checkInterval:  time.Second, // TODO make configurable
		done:           make(chan bool),
	}

	return p
}

func (p *PathWatcher) Run() {
	ticker := time.NewTicker(p.checkInterval)
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.check()
		}
	}
}

func (p *PathWatcher) Stop() {
	p.done <- true
	for _, tailer := range p.tailers {
		tailer.Stop()
	}
}

func (p *PathWatcher) check() {
	files, err := filepath.Glob(p.pattern)
	if err != nil {
		logrus.WithError(err).Error("Error globbing files")
	}
	current := make(map[string]struct{}, len(p.tailers))
	for _, file := range files {
		_, ok := p.tailers[file]
		if !ok {
			if p.filter != nil && !p.filter(file) {
				continue
			}
			handler := p.handlerFactory.New(file)
			tailer := NewTailer(file, handler, p.stateRecorder)
			p.tailers[file] = tailer
			go tailer.Run()
		}
		current[file] = struct{}{}
	}
	for file, tailer := range p.tailers {
		_, ok := current[file]
		if !ok {
			// If the file is gone, clean up its tailer.
			tailer.Stop()
			tailer.Clear()
			delete(p.tailers, file)
		}
	}
}
