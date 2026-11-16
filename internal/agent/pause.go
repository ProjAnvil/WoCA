package agent

// The pause card vocabulary drivers consume (graph-native HITL §3.4, audit
// fix M1): a pause carries EVERY interrupt of its superstep — the main
// graph's single ApprovalRequest and/or one interrupt per fanned-out child,
// each addressable by its fork namespace. Drivers render one card per
// interrupt and answer with a scalar (exactly one pending) or an NS-keyed
// map (several pending; unanswered cards re-pause, fork-native).

import (
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"
)

// PauseCard is one pending interrupt of a pause: its fork address and its
// decoded payload. NS is the interrupt's full task checkpoint namespace
// ("<node>:<taskID>" at the root, "<parentNS>/<node>:<taskID>" inside a
// subgraph) — the key an NS-keyed map answer addresses it by; ID is the
// interrupt id (the fallback key for legacy NS-less interrupts). Value is
// the decoded payload: ApprovalRequest for every interrupt the current
// topology raises (main and child alike — Origin separates them); the
// legacy single kinds survive for pre-Task-5 checkpoints.
type PauseCard struct {
	NS    string `json:"ns,omitempty"`
	ID    string `json:"id,omitempty"`
	Value any    `json:"value,omitempty"`
}

// MultiInterrupt is the goal relay's aggregate: the inner agent graph's
// whole pause (every interrupt with its fork address) surfaced as the outer
// goal graph's single interrupt value. The relay raises it only when the
// inner pause carries more than one interrupt; a single inner interrupt
// passes through unchanged (legacy goal pauses keep their exact shape).
type MultiInterrupt struct {
	Interrupts []PauseCard `json:"interrupts"`
}

// PauseCards builds the driver-facing card list of a pause. A card's value
// is decoded; a MultiInterrupt payload (the goal relay's aggregate) unwraps
// into the inner pause's cards so drivers see one flat list either way.
func PauseCards(intrs []lgtypes.Interrupt) []PauseCard {
	cards := make([]PauseCard, 0, len(intrs))
	for _, in := range intrs {
		value := decodeInterruptPayload(in.Value)
		if agg, ok := value.(MultiInterrupt); ok {
			for _, card := range agg.Interrupts {
				card.Value = decodeInterruptPayload(card.Value)
				cards = append(cards, card)
			}
			continue
		}
		cards = append(cards, PauseCard{NS: in.NS, ID: in.ID, Value: value})
	}
	return cards
}

// CardKey is the map key that addresses one card: its NS when present (the
// fork prefers NS addressing), else its interrupt ID (legacy checkpoints).
func CardKey(c PauseCard) string {
	if c.NS != "" {
		return c.NS
	}
	return c.ID
}
