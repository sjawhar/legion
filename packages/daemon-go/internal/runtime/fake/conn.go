package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var (
	_ runtime.Conn  = (*Conn)(nil)
	_ runtime.Conns = (*Conns)(nil)
)

// Prompt is one prompt frame the daemon sent, recorded whether the agent took it or refused it:
// a refused send is still a send, and the budget that counts them has to see it.
type Prompt struct {
	DeliveryID string
	Message    string
}

// Adoption is one working-copy adoption the daemon asked the agent's side to perform.
type Adoption struct {
	Identity runtime.GitIdentity
	Timeout  time.Duration
}

// Conn is one agent's connection as a double: it records what was asked of the agent and answers
// what the test told it to. The zero value is not usable; call NewConn.
type Conn struct {
	mu        sync.Mutex
	prompts   []Prompt
	adoptions []Adoption
	shutdowns int
	state     runtime.ConnState
	failures  map[string]error
}

// NewConn is a connection that accepts everything and reports no turn in flight.
func NewConn() *Conn {
	return &Conn{failures: map[string]error{}}
}

func (c *Conn) Prompt(_ context.Context, deliveryID, message string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prompts = append(c.prompts, Prompt{DeliveryID: deliveryID, Message: message})
	return c.failures["Prompt"]
}

func (c *Conn) GetState(_ context.Context) (runtime.ConnState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.failures["GetState"]; err != nil {
		return runtime.ConnState{}, err
	}
	return c.state, nil
}

func (c *Conn) Shutdown(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shutdowns++
	return c.failures["Shutdown"]
}

func (c *Conn) AdoptWorkingCopy(_ context.Context, id runtime.GitIdentity, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.adoptions = append(c.adoptions, Adoption{Identity: id, Timeout: timeout})
	return c.failures["AdoptWorkingCopy"]
}

// Prompts is every prompt frame sent over this connection, in order.
func (c *Conn) Prompts() []Prompt {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Prompt(nil), c.prompts...)
}

// Adoptions is every working-copy adoption asked over this connection, in order.
func (c *Conn) Adoptions() []Adoption {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Adoption(nil), c.adoptions...)
}

// Shutdowns is how many times the agent was asked to end itself.
func (c *Conn) Shutdowns() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shutdowns
}

// SetStreaming is the agent's turn starting or ending, as `GetState` would report it.
func (c *Conn) SetStreaming(streaming bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.IsStreaming = streaming
}

// FailPrompt makes every later Prompt return err: the transport failing under the frame.
// FailPrompt(nil) clears it, and clears a scripted refusal too.
func (c *Conn) FailPrompt(err error) { c.fail("Prompt", err) }

// RefusePrompt makes every later Prompt the agent refusing the frame: an error wrapping
// runtime.ErrPromptRefused, as the stream's connection answers `success: false`.
func (c *Conn) RefusePrompt(reason string) {
	c.fail("Prompt", fmt.Errorf("%s: %w", reason, runtime.ErrPromptRefused))
}

// FailGetState makes every later GetState return err.
func (c *Conn) FailGetState(err error) { c.fail("GetState", err) }

// FailShutdown makes every later Shutdown return err.
func (c *Conn) FailShutdown(err error) { c.fail("Shutdown", err) }

// FailAdoptWorkingCopy makes every later AdoptWorkingCopy return err.
func (c *Conn) FailAdoptWorkingCopy(err error) { c.fail("AdoptWorkingCopy", err) }

func (c *Conn) fail(method string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures[method] = err
}

// Conns is the connection directory as a double. The zero value is not usable; call NewConns.
type Conns struct {
	mu    sync.Mutex
	conns map[claim.Token]runtime.Conn
}

// NewConns is an empty directory: no claim has connected yet.
func NewConns() *Conns {
	return &Conns{conns: map[claim.Token]runtime.Conn{}}
}

func (c *Conns) Conn(token claim.Token) (runtime.Conn, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conn, ok := c.conns[token]
	return conn, ok
}

// Register is a claim's agent connecting.
func (c *Conns) Register(token claim.Token, conn runtime.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conns[token] = conn
}

// Remove is that connection going away.
func (c *Conns) Remove(token claim.Token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.conns, token)
}
