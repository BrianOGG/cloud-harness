// Package domain holds the Harness facts that every adapter must share:
// run and invocation states, model response items, tool-batch protocol,
// and budget arithmetic.
//
// Domain code is pure. It must not import HTTP, SQLite, a provider SDK,
// or any other internal package. Adapters convert at the edges.
package domain
