package backend

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/sensu/sensu-go/backend/messaging"
	"github.com/sensu/sensu-go/backend/store"
	"github.com/sensu/sensu-go/types"
)

// MessageScheduler is responsible for looping and publishing check requests for
// a given check.
type MessageScheduler struct {
	MessageBus messaging.MessageBus
	HashKey    []byte
	Interval   int
	MsgBytes   []byte
	Topics     []string

	stop chan struct{}
}

// Start the scheduling loop
func (s *MessageScheduler) Start() error {
	s.stop = make(chan struct{})
	sum := md5.Sum(s.HashKey)
	splayHash, n := binary.Uvarint(sum[0:7])
	if n < 0 {
		return errors.New("check hashing failed")
	}

	go func() {
		now := uint64(time.Now().UnixNano())
		// (splay_hash - current_time) % (interval * 1000) / 1000
		nextExecution := (splayHash - now) % (uint64(s.Interval) * uint64(1000))
		timer := time.NewTimer(time.Duration(nextExecution) * time.Millisecond)
		for {
			select {
			case <-timer.C:
				timer.Reset(time.Duration(time.Second * time.Duration(s.Interval)))
				for _, t := range s.Topics {
					if err := s.MessageBus.Publish(t, s.MsgBytes); err != nil {
						log.Println("error publishing check request: ", err.Error())
					}
				}
			case <-s.stop:
				timer.Stop()
				return
			}
		}
	}()
	return nil
}

// Stop stops the CheckScheduler
func (s *MessageScheduler) Stop() {
	if s.stop != nil {
		close(s.stop)
	}
}

func newSchedulerFromCheck(bus messaging.MessageBus, check *types.Check) (*MessageScheduler, error) {
	evt := &types.Event{
		Check: check,
	}
	evtBytes, err := json.Marshal(evt)
	if err != nil {
		return nil, err
	}
	scheduler := &MessageScheduler{
		MessageBus: bus,
		MsgBytes:   evtBytes,
		HashKey:    []byte(check.Name),
		Topics:     check.Subscriptions,
		Interval:   check.Interval,
	}
	return scheduler, nil
}

// Checker is responsible for managing check timers and publishing events to
// a messagebus
type Checker struct {
	Client     *clientv3.Client
	Store      store.Store
	MessageBus messaging.MessageBus

	schedulers map[string]*MessageScheduler
	wg         *sync.WaitGroup
	watcher    clientv3.Watcher
	errChan    chan error
}

// Start the Checker.
func (c *Checker) Start() error {
	if c.Client == nil {
		return errors.New("no etcd client available")
	}

	if c.Store == nil {
		return errors.New("no store available")
	}

	checks, err := c.Store.GetChecks()
	if err != nil {
		return err
	}

	c.schedulers = map[string]*MessageScheduler{}
	for _, check := range checks {
		scheduler, err := newSchedulerFromCheck(c.MessageBus, check)
		if err != nil {
			return err
		}
		scheduler.Start()
		c.schedulers[check.Name] = scheduler
	}

	watcher := clientv3.NewWatcher(c.Client)
	c.watcher = watcher
	c.errChan = make(chan error, 1)
	c.wg = &sync.WaitGroup{}
	c.wg.Add(1)
	c.startWatcher()

	return nil
}

func (c *Checker) startWatcher() {
	go func() {
		for resp := range c.watcher.Watch(context.TODO(), "/sensu.io/checks", clientv3.WithPrefix()) {
			for _, ev := range resp.Events {
				switch ev.Type {
				case mvccpb.PUT:
					check := &types.Check{}
					err := json.Unmarshal(ev.Kv.Value, check)
					if err != nil {
						log.Printf("error unmarshalling check \"%s\": %s", string(ev.Kv.Value), err.Error())
						continue
					}
					sched, ok := c.schedulers[check.Name]
					if ok {
						sched.Stop()
					}
					newScheduler, err := newSchedulerFromCheck(c.MessageBus, check)
					if err != nil {
						log.Println("error creating new check scheduler: ", err.Error())
					}
					err = newScheduler.Start()
					if err != nil {
						log.Println("error starting new check scheduler: ", err.Error())
					}
				case mvccpb.DELETE:
					parts := strings.Split(string(ev.Kv.Key), "/")
					name := parts[len(parts)-1]
					delete(c.schedulers, name)
				}
			}
		}
		c.wg.Done()
	}()
}

// Stop the Checker.
func (c *Checker) Stop() error {
	if err := c.watcher.Close(); err != nil {
		return err
	}
	// let the event queue drain so that we don't panic inside the loop.
	// TODO(greg): get ride of this dependency.
	c.wg.Wait()

	return nil
}

// Status returns the health of the Checker.
func (c *Checker) Status() error {
	return nil
}

// Err returns a channel on which to listen for terminal errors.
func (c *Checker) Err() <-chan error {
	return c.errChan
}
