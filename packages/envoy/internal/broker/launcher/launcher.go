// Package launcher will issue launcher credentials through a Dispatch standing-issue ask (see
// AGENTC-833 Task 12). This file is a placeholder: Task 11's server.go (given verbatim by its own
// brief) already declares api.Deps.Launcher *launcher.Service and imports this package, so the
// type must exist for that file to compile even though Task 12, which owns launcher.go, has not
// landed yet. Service carries no state and api.Deps.Launcher is left nil by every Task 11 wiring
// path; the two authNone launcher-credential routes answer 501 until Task 12 replaces this file
// with the real Store/Dispatch/Enroll/Project/Standing fields and Request/Read/Reconcile methods
// described in its own brief.
package launcher

// Service is a placeholder for Task 12's launcher.Service.
type Service struct{}
