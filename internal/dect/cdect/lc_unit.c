// SPDX-License-Identifier: AGPL-3.0-or-later
// Compiles libopendectcell's Lc layer into this package: cgo only builds C files in
// the package directory, so each library source is included from the submodule.
#include "../../../third_party/opendectcell/src/libopendectcell/dect_lc.c"
