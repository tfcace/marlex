// Package rex holds mx's Lua module for the Rex terminal.
package rex

import _ "embed"

// Module is mx.lua: blame anything on screen, from a key.
//
//go:embed mx.lua
var Module string

// Watch is watch.lua, which `mx why -i --follow` runs with rex do to
// see a terminal's screen as it changes.
//
//go:embed watch.lua
var Watch string
