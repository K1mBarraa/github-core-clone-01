package main

// Forge entry point: bring the app server up, tell the trial environment
// where to find it, and stay alive until the environment says the trial
// is over.
//
// The sequence, in order:
//   * read MAKEFUN_ENV_URL, the trial environment's address. Without it
//     there is nobody to talk to, so quit straight away;
//   * start the app server on a port the kernel picks — never a fixed
//     one, because the environment itself owns 8080;
//   * POST the picked port to <env>/register so the environment can
//     dial back to this process;
//   * keep asking <env>/done until it answers that the trial finished;
//   * shut the server down and leave with status 0.
//
// The environment can be slow to wake, so every request is retried with
// a pause that grows between attempts: 250 ms at first, doubling each
// time, never longer than 2 s. A 200 or a 409 from /register both mean
// the port is recorded; a 400 or a 405 means the request itself is wrong
// and retrying it is pointless.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// requestBudget is the longest a single HTTP round trip may take before
// it counts as a failed attempt and gets retried.
const requestBudget = 30 * time.Second

// The retry pause ladder: start here, double after each failure, never
// climb past the ceiling. The step cap exists so the ladder stops growing
// once it is already past the ceiling anyway.
const (
	backoffStart   = 250 * time.Millisecond
	backoffCeiling = 2 * time.Second
	backoffMaxStep = 8
)

// backoffMs returns the pause after `failures` failed attempts.
func backoffMs(failures int) time.Duration {
	step := failures
	if step > backoffMaxStep {
		step = backoffMaxStep
	}
	pause := backoffStart << uint(step)
	if pause > backoffCeiling {
		pause = backoffCeiling
	}
	return pause
}

// verdict is what one probe attempt can report back to the retry loop.
type verdict int

const (
	verdictOK verdict = iota
	verdictFatal
	verdictAgain
)

// retryUntilOk calls probe over and over until it reports success. A
// probe that returns an error counts as a failed attempt, not as the end
// of the world: the environment may simply not be answering yet. Only a
// "fatal" verdict stops the loop, because then another attempt could
// never succeed.
func retryUntilOk(label string, probe func() (verdict, error)) error {
	failures := 0
	for {
		v, err := probe()
		if err != nil {
			v = verdictAgain
		}
		if v == verdictOK {
			return nil
		}
		if v == verdictFatal {
			return fmt.Errorf("%s was rejected by the trial env", label)
		}
		time.Sleep(backoffMs(failures))
		failures++
	}
}

// announce tells the trial environment which port this process listens on.
func announce(client *http.Client, envBase string, port int) error {
	payload, err := json.Marshal(map[string]any{"port": port})
	if err != nil {
		return err
	}
	return retryUntilOk("register", func() (verdict, error) {
		req, err := http.NewRequest(http.MethodPost, envBase+"/register", bytes.NewReader(payload))
		if err != nil {
			return verdictAgain, err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return verdictAgain, err
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		switch res.StatusCode {
		case http.StatusOK, http.StatusConflict:
			return verdictOK, nil
		case http.StatusBadRequest, http.StatusMethodNotAllowed:
			return verdictFatal, nil
		default:
			return verdictAgain, nil
		}
	})
}

// waitUntilDone asks the trial environment whether the trial is over yet.
// It never returns until the answer is yes.
func waitUntilDone(client *http.Client, envBase string) error {
	return retryUntilOk("done", func() (verdict, error) {
		res, err := client.Get(envBase + "/done")
		if err != nil {
			return verdictAgain, err
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			_, _ = io.Copy(io.Discard, res.Body)
			return verdictAgain, nil
		}
		var payload struct {
			Done bool `json:"done"`
		}
		if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
			return verdictAgain, err
		}
		if payload.Done {
			return verdictOK, nil
		}
		return verdictAgain, nil
	})
}

// run is the whole life of the process: bind, announce, wait, shut down.
func run() error {
	// The trial environment's address, with any trailing slashes removed.
	envBase := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envBase == "" {
		return errors.New("MAKEFUN_ENV_URL is missing, so there is no trial env to join")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return errors.New("the server did not get a TCP port from the kernel")
	}
	port := addr.Port

	server := newRawServer(CreateApp(), listener)
	// Guarantee the listener is closed on every exit path, not just the
	// happy path after /done answers.
	defer func() { _ = server.Close() }()
	serveErr := make(chan error, 1)
	go func() {
		if err := server.Serve(); err != nil {
			serveErr <- err
		}
	}()

	fmt.Printf("[forge] listening on port %d\n", port)

	client := &http.Client{Timeout: requestBudget}
	if err := announce(client, envBase, port); err != nil {
		return err
	}
	fmt.Println("[forge] registered with the trial env")

	if err := waitUntilDone(client, envBase); err != nil {
		return err
	}
	_ = server.Close()
	fmt.Println("[forge] trial finished, shutting down")

	select {
	case err := <-serveErr:
		return err
	default:
		return nil
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] %s\n", err.Error())
		os.Exit(1)
	}
}
