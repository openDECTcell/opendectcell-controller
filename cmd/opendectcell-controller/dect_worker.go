// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/worker"
)

// cellWorker is the `cellworker` command: a worker process of the DECT stacks (ADR 0003).
// serve starts it with the socket to the controller as file 3, nothing else
// inherited: it has no database, no master key and no network.
func cellWorker() error {
	if dectStack == nil {
		return errors.New("cellworker: this binary has no libopendectcell")
	}
	worker.Harden()
	f := os.NewFile(3, "controller")
	conn, err := net.FileConn(f)
	if err != nil {
		return fmt.Errorf("cellworker: file 3 is not the controller's socket: %w", err)
	}
	f.Close()
	// from here on this process may open no file and no connection, and start no program
	if err := worker.Confine(); err != nil {
		return fmt.Errorf("cellworker: cannot confine the process: %w", err)
	}
	return worker.Serve(conn, dectStack)
}
