// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !libopendectcell

package main

import "github.com/openDECTcell/opendectcell-controller/internal/dect/stack"

// dectStack is nil: this binary was built without libopendectcell, so the DECT
// core is off and the cells' mails are counted and dropped.
var dectStack stack.New
