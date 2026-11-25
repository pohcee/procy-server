package chserver

import (
	"encoding/json"
	"sync"
	"time"
)

// defaultOutputWait bounds how long a job result waits for the job's output channels to end. A client sends the
// result right before it closes them, so the wait is short; the bound keeps a broken channel from holding results.
const defaultOutputWait = 5 * time.Second

// outputStreams counts the output channels (stdout, stderr) still open per job: a client sends a job's result
// as a request, handled apart from the channels, so a fast command's result can arrive before its output was
// written to the UI. The result waits for the job's channels to end (outputDone), so the UI gets every output
// frame before the result, which ends its stream.
type outputStreams struct {
	mu   sync.Mutex
	jobs map[string]*openOutputs
}

type openOutputs struct {
	n    int
	done chan struct{}
}

func (o *outputStreams) open(jid string) func() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.jobs == nil {
		o.jobs = map[string]*openOutputs{}
	}
	e := o.jobs[jid]
	if e == nil {
		e = &openOutputs{done: make(chan struct{})}
		o.jobs[jid] = e
	}
	e.n++
	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			e.n--
			if e.n == 0 {
				close(e.done)
				if o.jobs[jid] == e {
					delete(o.jobs, jid)
				}
			}
		})
	}
}

// wait blocks until the open output channels of jid end, at most max.
func (o *outputStreams) wait(jid string, max time.Duration) bool {
	o.mu.Lock()
	e := o.jobs[jid]
	o.mu.Unlock()
	if e == nil {
		return true
	}
	t := time.NewTimer(max)
	defer t.Stop()
	select {
	case <-e.done:
		return true
	case <-t.C:
		return false
	}
}

// outputOpened records an output channel of the job in jobData (the channel's extra data) until the returned
// function is called. Call it before accepting the channel, so a result arriving later finds it.
func (cl *ClientListener) outputOpened(jobData []byte) func() {
	var job struct {
		JID string `json:"jid"`
	}
	if err := json.Unmarshal(jobData, &job); err != nil || job.JID == "" {
		return func() {}
	}
	return cl.outputs.open(job.JID)
}

// awaitOutput waits for the output channels of job jid to end (bounded).
func (cl *ClientListener) awaitOutput(jid string) {
	max := cl.outputWait
	if max <= 0 {
		max = defaultOutputWait
	}
	if !cl.outputs.wait(jid, max) {
		cl.log().Debugf("job %s: output still open %s after its result; the result is sent", jid, max)
	}
}
