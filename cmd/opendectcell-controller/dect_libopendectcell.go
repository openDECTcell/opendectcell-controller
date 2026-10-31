// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build libopendectcell

package main

import (
	"github.com/openDECTcell/opendectcell-controller/internal/dect/cdect"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// dectStack is libopendectcell's base stack: this binary was built with the
// openDECTcell submodule and cgo (`-tags libopendectcell`).
var dectStack stack.New = cdect.NewStack
