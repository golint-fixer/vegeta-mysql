package vegeta

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Attacker is an attack executor which wraps an http.Client
type Attacker struct {
	//client       http.Client
	cnn          *sql.DB
	stopch       chan struct{}
	workers      uint64
	redirects    int
	maxIdleConns int
	maxOpenConns int
	persistent   bool
	dsn          string
}

const (
	// DefaultTimeout is the default amount of time an Attacker waits for a request
	// before it times out.
	DefaultTimeout = 30 * time.Second
	// DefaultConnections is the default amount of max open idle connections per
	// target host.
	DefaultConnections = 10000
	// DefaultWorkers is the default initial number of workers used to carry an attack.
	DefaultWorkers = 10
	// NoFollow is the value when redirects are not followed but marked successful
	NoFollow = -1
	// Persistent connection
	DefaultPersistent = true
)

var (
// DefaultTLSConfig is the default tls.Config an Attacker uses.
)

// NewAttacker returns a new Attacker with default options which are overridden
// by the optionally provided opts.
func NewAttacker(opts ...func(*Attacker)) *Attacker {
	a := &Attacker{stopch: make(chan struct{}), workers: DefaultWorkers}
	for _, opt := range opts {
		opt(a)
	}
	var err error
	if a.persistent {
		a.cnn, err = sql.Open("mysql", a.dsn)
		if err != nil {
			log.Fatal(err)
		}
		a.cnn.SetMaxIdleConns(a.maxIdleConns)
		a.cnn.SetMaxOpenConns(a.maxOpenConns)
		a.cnn.Ping()
	}
	return a
}

// Workers returns a functional option which sets the initial number of workers
// an Attacker uses to hit its targets. More workers may be spawned dynamically
// to sustain the requested rate in the face of slow responses and errors.
func Workers(n uint64) func(*Attacker) {
	return func(a *Attacker) { a.workers = n }
}

func Dsn(s string) func(*Attacker) {
	return func(a *Attacker) { a.dsn = s }
}

func SetPersistent(n bool) func(*Attacker) {
	return func(a *Attacker) { a.persistent = n }
}
func SetMaxIdleConns(n int) func(*Attacker) {
	return func(a *Attacker) { a.maxIdleConns = n }
}
func SetMaxOpenConns(n int) func(*Attacker) {
	return func(a *Attacker) { a.maxOpenConns = n }
}

// Attack reads its Targets from the passed Targeter and attacks them at
// the rate specified for duration time. Results are put into the returned channel
// as soon as they arrive.
func (a *Attacker) Attack(tr Targeter, rate uint64, du time.Duration) <-chan *Result {
	workers := &sync.WaitGroup{}
	results := make(chan *Result)
	ticks := make(chan time.Time)
	for i := uint64(0); i < a.workers; i++ {
		go a.attack(tr, workers, ticks, results)
	}

	go func() {
		defer close(results)
		defer workers.Wait()
		defer close(ticks)
		interval := 1e9 / rate
		hits := rate * uint64(du.Seconds())
		for began, done := time.Now(), uint64(0); done < hits; done++ {
			now, next := time.Now(), began.Add(time.Duration(done*interval))
			time.Sleep(next.Sub(now))
			select {
			case ticks <- max(next, now):
			case <-a.stopch:
				return
			default: // all workers are blocked. start one more and try again
				go a.attack(tr, workers, ticks, results)
				done--
			}
		}
	}()

	return results
}

// Stop stops the current attack.
func (a *Attacker) Stop() { close(a.stopch) }

func (a *Attacker) attack(tr Targeter, workers *sync.WaitGroup, ticks <-chan time.Time, results chan<- *Result) {
	workers.Add(1)
	defer workers.Done()
	for tm := range ticks {
		results <- a.hit(tr, tm)
	}
}

func (a *Attacker) hit(tr Targeter, tm time.Time) *Result {
	var (
		res = Result{Timestamp: tm}
		tgt Target
		err error
	)

	defer func() {
		res.Latency = time.Since(tm)
		if err != nil {
			res.Error = err.Error()
		}
	}()

	if err = tr(&tgt); err != nil {
		return &res
	}

	req, err := tgt.Query()
	if err != nil {
		return &res
	}

	/*
		a.cnn, err = sql.Open("mysql", a.dsn)
		if err != nil {
			res.Code = 500
			res.Error = err.Error()
			return &res
		}
		defer a.cnn.Close()
		a.cnn.SetMaxIdleConns(a.maxIdleConns)
		a.cnn.SetMaxOpenConns(a.maxOpenConns)
	*/
	//log.Printf("query:%s", req)

	var r *sql.Rows
	if a.persistent {
		r, err = a.cnn.Query(req)
	} else {
		var cnn *sql.DB
		cnn, err = sql.Open("mysql", a.dsn)
		cnn.SetMaxIdleConns(0)
		cnn.SetMaxOpenConns(1)
		cnn.SetConnMaxLifetime(1 * time.Nanosecond)
		if err != nil {
			res.Code = 500
			res.Error = err.Error()
			return &res
		}
		r, err = cnn.Query(req)
	}
	if err != nil {
		// ignore redirect errors when the user set --redirects=NoFollow
		if a.redirects == NoFollow && strings.Contains(err.Error(), "stopped after") {
			err = nil
		}
		return &res
	}
	defer r.Close()
	num := 0
	for r.Next() {
		num++
		/*
			var id interface{}
				if err := r.Scan(&id); err != nil {
					res.Code = 501
					res.Error = err.Error()
					res.Error = fmt.Sprintf("row scan err:%s: query:%s", err.Error(), req)
					return &res
				}
		*/
	}
	//fmt.Fprintf(os.Stderr, "%d,", num)
	res.BytesIn = 0
	res.BytesOut = 0

	err = r.Err()

	if err == nil {
		if num == 0 {
			res.Code = 201
		} else {
			res.Code = 200
		}
	} else {
		res.Code = 500
		res.Error = fmt.Sprintf("%s: query:%s", err.Error(), req)
	}
	return &res
}

func max(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
