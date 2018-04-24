package borges

import (
	"io"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/src-d/borges/metrics"
	"gopkg.in/src-d/framework.v0/queue"
)

// Producer is a service to generate jobs and put them to the queue.
type Producer struct {
	log           *logrus.Entry
	jobIter       JobIter
	queue         queue.Queue
	running       bool
	maxJobRetries int
	priority      queue.Priority
	startOnce     *sync.Once
	stopOnce      *sync.Once

	// used by Stop() to wait until Start() has finished
	startIsRunning chan struct{}
}

// NewProducer creates a new producer.
func NewProducer(
	log *logrus.Entry,
	jobIter JobIter,
	queue queue.Queue,
	priority queue.Priority,
	jobRetries int,
) *Producer {
	return &Producer{
		log:           log,
		jobIter:       jobIter,
		queue:         queue,
		maxJobRetries: jobRetries,
		priority:      priority,
		startOnce:     &sync.Once{},
		stopOnce:      &sync.Once{},
	}
}

// Start starts the producer services. It blocks until Stop is called.
func (p *Producer) Start() {
	p.startOnce.Do(p.start)
}

// Stop stops the producer.
func (p *Producer) Stop() {
	p.stopOnce.Do(p.stop)
}

func (p *Producer) start() {
	log := p.log
	log.Info("starting up")

	p.running = true
	p.startIsRunning = make(chan struct{})
	defer func() { close(p.startIsRunning) }()

	const nextJobErrMaxPrints = 5
	var nextJobSameErr int
	var lastNextJobErr error
	for {
		if !p.running {
			break
		}

		j, err := p.jobIter.Next()
		if err == io.EOF {
			log.Info("no more jobs in the queue")
			break
		}

		if ErrWaitForJobs.Is(err) {
			time.Sleep(time.Millisecond * 500)
			continue
		}

		if err != nil {
			if nextJobSameErr < nextJobErrMaxPrints {
				log.WithField("error", err).Error("error obtaining next job")
				if lastNextJobErr == nil || err.Error() == lastNextJobErr.Error() {
					nextJobSameErr++
				} else {
					nextJobSameErr = 0
				}
			}

			lastNextJobErr = err
			continue
		}

		nextJobSameErr = 0

		if err := p.add(j); err != nil {
			metrics.RepoProduceFailed()
			log.WithFields(logrus.Fields{"job": j.RepositoryID, "error": err}).Error("error adding job to the queue")
		} else {
			metrics.RepoProduced()
			log.WithField("job", j.RepositoryID).Info("job queued")
		}
	}

	log.Info("stopping")
}

func (p *Producer) add(j *Job) error {
	qj, err := queue.NewJob()
	if err != nil {
		return err
	}

	qj.Retries = int32(p.maxJobRetries)
	if err := qj.Encode(j); err != nil {
		return err
	}

	qj.SetPriority(p.priority)

	return p.queue.Publish(qj)
}

func (p *Producer) stop() {
	p.running = false
	p.closeIter()
	<-p.startIsRunning
}

func (p *Producer) closeIter() {
	if p.jobIter == nil {
		return
	}

	if err := p.jobIter.Close(); err != nil {
		p.log.WithField("error", err).Error("error closing queue iterator")
	}

	p.jobIter = nil
}
