package processor

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"pr-agent-go/internal/orchestrator"
)

type Snapshot struct {
	WorkerCount    int      `json:"workerCount"`
	QueueSize      int      `json:"queueSize"`
	Queued         int64    `json:"queued"`
	Active         int64    `json:"active"`
	Completed      int64    `json:"completed"`
	Failed         int64    `json:"failed"`
	LastSuccessAt  string   `json:"lastSuccessAt,omitempty"`
	LastFailureAt  string   `json:"lastFailureAt,omitempty"`
	RecentFailures []string `json:"recentFailures"`
}

type Processor struct {
	service        *orchestrator.Service
	jobs           chan orchestrator.WebhookEnvelope
	workerCount    int
	queueSize      int
	active         atomic.Int64
	completed      atomic.Int64
	failed         atomic.Int64
	mu             sync.Mutex
	lastSuccessAt  string
	lastFailureAt  string
	recentFailures []string
}

func New(service *orchestrator.Service, workerCount, queueSize int) *Processor {
	if workerCount <= 0 {
		workerCount = 1
	}
	if queueSize <= 0 {
		queueSize = 1
	}

	p := &Processor{
		service:     service,
		jobs:        make(chan orchestrator.WebhookEnvelope, queueSize),
		workerCount: workerCount,
		queueSize:   queueSize,
	}
	for i := 0; i < workerCount; i++ {
		go p.worker()
	}
	return p
}

func (p *Processor) Enqueue(job orchestrator.WebhookEnvelope) error {
	select {
	case p.jobs <- job:
		return nil
	default:
		return fmt.Errorf("queue_full")
	}
}

func (p *Processor) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	failures := make([]string, len(p.recentFailures))
	copy(failures, p.recentFailures)

	return Snapshot{
		WorkerCount:    p.workerCount,
		QueueSize:      p.queueSize,
		Queued:         int64(len(p.jobs)),
		Active:         p.active.Load(),
		Completed:      p.completed.Load(),
		Failed:         p.failed.Load(),
		LastSuccessAt:  p.lastSuccessAt,
		LastFailureAt:  p.lastFailureAt,
		RecentFailures: failures,
	}
}

func (p *Processor) worker() {
	for job := range p.jobs {
		p.active.Add(1)
		result, err := p.service.HandleWebhook(job)
		p.active.Add(-1)

		p.mu.Lock()
		if err != nil {
			p.failed.Add(1)
			p.lastFailureAt = time.Now().UTC().Format(time.RFC3339)
			p.recentFailures = append([]string{err.Error()}, p.recentFailures...)
			if len(p.recentFailures) > 5 {
				p.recentFailures = p.recentFailures[:5]
			}
		} else {
			p.completed.Add(1)
			p.lastSuccessAt = time.Now().UTC().Format(time.RFC3339)
			log.Printf("webhook processed repo=%s pr=%d action=%s/%s timings=%v", result.RepoFullName, result.PRNumber, result.ActionTaken, result.ActionStatus, result.StageDurationsMS)
		}
		p.mu.Unlock()
	}
}
