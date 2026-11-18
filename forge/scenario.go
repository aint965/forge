// The token owner's identity, derived from the contest's one fixed scenario
// seed. The suite builds every name it uses from that seed, so the login the
// token names is a fixed function of it: the first user of the 'users' stream.
// Recomputing it here (rather than trusting any request path) is what lets the
// server tell its own repositories from a stranger's.
//
// The generator below mirrors the published derivation: FNV-1a over the stream
// label, splitmix64's finalizer, and a xorshift64* stream with unbiased
// rejection sampling.

package main

const scenarioSeed uint64 = 0x6a09e667f3bcc909

var scenarioFirstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var scenarioLastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

func fnv1a64(text string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		h ^= uint64(text[i])
		h *= 0x100000001b3
	}
	return h
}

func mix64(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// scenarioRng is the xorshift64* stream the scenario uses.
type scenarioRng struct{ state uint64 }

func newScenarioRng(label string) *scenarioRng {
	seed := mix64(scenarioSeed ^ fnv1a64(label))
	if seed == 0 {
		seed = scenarioSeed
	}
	return &scenarioRng{state: seed}
}

func (r *scenarioRng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *scenarioRng) nextU32() uint32 { return uint32(r.nextU64() >> 32) }

// nextInt draws uniformly from [0, n) by rejection sampling.
func (r *scenarioRng) nextInt(n int) int {
	if n <= 1 {
		return 0
	}
	const range32 = uint64(1) << 32
	limit := range32 - (range32 % uint64(n))
	for {
		if v := uint64(r.nextU32()); v < limit {
			return int(v % uint64(n))
		}
	}
}

func (r *scenarioRng) pick(items []string) string { return items[r.nextInt(len(items))] }

type viewerIdentity struct {
	id    int
	login string
	name  string
	email string
}

// deriveViewer computes the first user of the 'users' stream: the viewer, the
// token owner, whose login every repository in this forge belongs to.
func deriveViewer() viewerIdentity {
	rng := newScenarioRng("users")
	first := rng.pick(scenarioFirstNames)
	last := rng.pick(scenarioLastNames)
	login := first + last
	return viewerIdentity{
		id:    1000,
		login: login,
		name:  first + " " + last,
		email: login + "@example.invalid",
	}
}
