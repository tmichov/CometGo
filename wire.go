package cometgo

import "encoding/json"

// Subprotocol is the WebSocket subprotocol for wire protocol v1 with JSON encoding.
const Subprotocol = "cometgo.1.json"

// clientMsg is the union of every client→server message. Fields are
// interpreted according to T. See PROTOCOL.md §3–§5.
type clientMsg struct {
	T string `json:"t"`

	// hello
	V      int          `json:"v"`
	Caps   []string     `json:"caps"`
	Auth   *helloAuth   `json:"auth"`
	Build  string       `json:"build"`
	Resume *helloResume `json:"resume"`

	// auth, call
	ID *uint64 `json:"id"`
	// auth: a JSON string, or null for logout. Kept raw to tell null from absent.
	Token json.RawMessage `json:"token"`

	// sub, unsub
	S    *uint64         `json:"s"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	Have bool            `json:"have"`

	// call
	M       string  `json:"m"`
	NoFence bool    `json:"noFence"`
	IK      *string `json:"ik"`

	// ping
	N json.RawMessage `json:"n"`

	// ack
	Seq *uint64 `json:"seq"`
}

type helloAuth struct {
	Token string `json:"token"`
}

// helloResume asks to resume a session after seq (§7.2). Subs lists the
// sub ids the client still holds.
type helloResume struct {
	Session string   `json:"session"`
	Seq     uint64   `json:"seq"`
	Subs    []uint64 `json:"subs"`
}

type limitsMsg struct {
	MaxSubs     int `json:"maxSubs"`
	MaxInflight int `json:"maxInflight"`
	MaxMsg      int `json:"maxMsg"`
}

type welcomeMsg struct {
	T       string    `json:"t"`
	Session string    `json:"session"`
	Epoch   string    `json:"epoch"`
	Resumed bool      `json:"resumed"`
	HB      int64     `json:"hb"`
	User    any       `json:"user"`
	AuthExp int64     `json:"authExp,omitempty"`
	Limits  limitsMsg `json:"limits"`
	Build   string    `json:"build,omitempty"`
	Caps    []string  `json:"caps"`
}

// sequenced is implemented by every stateful server→client message (§2).
type sequenced interface {
	setSeq(uint64)
}

type txMsg struct {
	T    string     `json:"t"`
	Seq  uint64     `json:"seq"`
	Subs []subEntry `json:"subs"`
	More bool       `json:"more,omitempty"`
}

func (m *txMsg) setSeq(s uint64) { m.Seq = s }

type subInit struct {
	Cols    [][2]string `json:"cols"`
	Key     []int       `json:"key"`
	Ordered bool        `json:"ordered"`
	Src     string      `json:"src,omitempty"`
}

type subEntry struct {
	S     uint64            `json:"s"`
	Init  *subInit          `json:"init,omitempty"`
	Reset bool              `json:"reset,omitempty"`
	Ops   []json.RawMessage `json:"ops,omitempty"`
	Ready bool              `json:"ready,omitempty"`
	End   *Error            `json:"end,omitempty"`
}

type resMsg struct {
	T   string          `json:"t"`
	Seq uint64          `json:"seq"`
	ID  uint64          `json:"id"`
	OK  bool            `json:"ok"`
	Val json.RawMessage `json:"val,omitempty"`
	Err *Error          `json:"err,omitempty"`
	Lag bool            `json:"lag,omitempty"`
}

func (m *resMsg) setSeq(s uint64) { m.Seq = s }

type authResMsg struct {
	T       string  `json:"t"`
	Seq     uint64  `json:"seq"`
	ID      *uint64 `json:"id,omitempty"`
	OK      bool    `json:"ok"`
	User    any     `json:"user"`
	AuthExp int64   `json:"authExp,omitempty"`
	Err     *Error  `json:"err,omitempty"`
}

func (m *authResMsg) setSeq(s uint64) { m.Seq = s }

// goawayMsg announces a graceful shutdown (§7.3). It is not stateful: it
// carries no seq and is never replayed.
type goawayMsg struct {
	T         string `json:"t"`
	Reason    string `json:"reason"`
	RetryInMs int64  `json:"retryInMs"`
}

type pongMsg struct {
	T string          `json:"t"`
	N json.RawMessage `json:"n,omitempty"`
}
