// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package graph

import "sort"

// resolution is the modularity resolution parameter. 1.0 is standard modularity;
// higher values bias toward smaller communities. It is the principled knob for the
// "oversized community" problem (preferred over a post-hoc split heuristic).
const resolution = 1.0

// DetectCommunities partitions the graph with the Louvain method (greedy
// modularity maximization) and writes a community id onto every node. Louvain
// beats the old label-propagation default on the quality that matters here: it
// does not collapse weakly-bridged clusters into one giant community, so the
// orientation tools and the web-graph coloring show meaningful groups. The result
// is deterministic (nodes are processed in sorted-id order, ties never move a
// node) and communities are renumbered 0..k-1 by their smallest member id, so the
// output shape is identical to the previous implementation; only the groupings
// improve. maxRounds caps the local-moving passes per level (default 20). Returns
// the number of communities.
func (g *Graph) DetectCommunities(maxRounds int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if maxRounds <= 0 {
		maxRounds = 20
	}

	// Cluster over the semantic nodes only (notes, tags, externals). Heading nodes
	// are structural children of a single note (a note -> heading "contains" edge),
	// so including them just hangs a private degree-1 pendant off every note, which
	// biases modularity toward splitting each note into its own community. A heading
	// instead inherits its parent note's community, so a heading is always shown in
	// its note's cluster. This is what lets genuinely connected notes group together
	// instead of each note + its heading forming a singleton.
	ids := make([]string, 0, len(g.nodes))
	var headings []*Node
	for id, n := range g.nodes {
		if n.Kind == "heading" {
			headings = append(headings, n)
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Sort headings too: the orphan fallback below assigns ids in slice order, and
	// the slice was built from a map range (random order), so without this two
	// orphan headings could swap community ids across runs (breaks determinism).
	sort.Slice(headings, func(i, j int) bool { return headings[i].ID < headings[j].ID })
	if len(ids) == 0 {
		// Unreachable via the real parse pipeline: a heading is only ever added after
		// its parent note (a cluster node), so a non-empty graph always has >=1
		// cluster node. Only a hand-built all-headings graph hits this; such headings
		// keep Community 0, which no consumer reads (all exclude headings).
		return 0
	}

	a := g.buildAdjacency(ids)
	comm := a.louvain(maxRounds)

	// Renumber to contiguous ids ordered by each community's smallest member id.
	// ids is sorted, so index order is smallest-id order: the first index seen for
	// a community owns the lowest member id.
	remap := make(map[int]int, len(ids))
	next := 0
	for i := range ids {
		if _, ok := remap[comm[i]]; !ok {
			remap[comm[i]] = next
			next++
		}
	}
	for i, id := range ids {
		g.nodes[id].Community = remap[comm[i]]
	}

	// Headings inherit their parent note's community; an orphan heading (parent not
	// in the graph) gets its own community so it is never silently lumped into 0.
	for _, h := range headings {
		if parent, ok := g.nodes["note:"+h.NoteID]; ok {
			h.Community = parent.Community
		} else {
			h.Community = next
			next++
		}
	}
	return next
}

// adjacency is an undirected, weighted view of the graph keyed by a dense node
// index (0..n-1, in sorted-id order). adj[i][j] is the combined weight of every
// edge between i and j in either direction; self holds a node's self-loop weight;
// k is the weighted degree (self-loops counted twice); twoM is the sum of all
// weighted degrees (= 2m).
type adjacency struct {
	n    int
	adj  []map[int]float64
	self []float64
	k    []float64
	twoM float64
}

func (g *Graph) buildAdjacency(ids []string) *adjacency {
	idx := make(map[string]int, len(ids))
	for i, id := range ids {
		idx[id] = i
	}
	a := &adjacency{
		n:    len(ids),
		adj:  make([]map[int]float64, len(ids)),
		self: make([]float64, len(ids)),
		k:    make([]float64, len(ids)),
	}
	for i := range a.adj {
		a.adj[i] = map[int]float64{}
	}
	// Each directed edge is stored once in g.adj[source], so iterating it visits
	// every edge exactly once; fold it into the symmetric undirected weight.
	for _, src := range ids {
		u := idx[src]
		for _, e := range g.adj[src] {
			v, ok := idx[e.Target]
			if !ok {
				continue // edge to a node not in the set (defensive)
			}
			w := e.Weight
			if w <= 0 {
				w = 1
			}
			if u == v {
				a.self[u] += w
				continue
			}
			a.adj[u][v] += w
			a.adj[v][u] += w
		}
	}
	for i := 0; i < a.n; i++ {
		sum := 2 * a.self[i]
		for _, w := range a.adj[i] {
			sum += w
		}
		a.k[i] = sum
		a.twoM += sum
	}
	return a
}

// louvain returns a community label per node index. It runs local moving then
// aggregation, repeating across levels until a level merges nothing.
func (a *adjacency) louvain(maxRounds int) []int {
	// orig2node maps an original node index to its node in the current (possibly
	// aggregated) level graph; comm accumulates the final community per orig node.
	orig2node := make([]int, a.n)
	for i := range orig2node {
		orig2node[i] = i
	}
	level := a
	for {
		if level.twoM == 0 {
			break // no edges: every node stays in its own community
		}
		comm, merged := level.localMoving(maxRounds)
		if !merged {
			break
		}
		// Compact community labels to 0..c-1 and thread the mapping back to the
		// original nodes.
		relabel := map[int]int{}
		next := 0
		for _, c := range comm {
			if _, ok := relabel[c]; !ok {
				relabel[c] = next
				next++
			}
		}
		for i := range orig2node {
			orig2node[i] = relabel[comm[orig2node[i]]]
		}
		if next == level.n {
			break // no reduction; converged
		}
		level = level.aggregate(comm, relabel, next)
	}
	return orig2node
}

// localMoving runs the greedy phase: each node (in index order) is moved to the
// neighbor community that most increases modularity, until a pass moves nothing.
// Returns the per-node community and whether any node ended up merged with another.
func (a *adjacency) localMoving(maxRounds int) (comm []int, merged bool) {
	comm = make([]int, a.n)
	commTot := make([]float64, a.n) // Sigma-tot per community (indexed by community id)
	for i := 0; i < a.n; i++ {
		comm[i] = i
		commTot[i] = a.k[i]
	}
	const eps = 1e-12
	for round := 0; round < maxRounds; round++ {
		moved := false
		for i := 0; i < a.n; i++ {
			ci := comm[i]
			commTot[ci] -= a.k[i] // remove i from its community

			// Sum edge weight from i into each neighbor community.
			into := map[int]float64{}
			for j, w := range a.adj[i] {
				into[comm[j]] += w
			}
			// Candidate order is deterministic; start by staying in ci, and only a
			// strictly-better gain moves i, so ties never cause churn.
			best := ci
			bestGain := into[ci] - resolution*commTot[ci]*a.k[i]/a.twoM
			cands := make([]int, 0, len(into))
			for c := range into {
				cands = append(cands, c)
			}
			sort.Ints(cands)
			for _, c := range cands {
				gain := into[c] - resolution*commTot[c]*a.k[i]/a.twoM
				if gain > bestGain+eps {
					best, bestGain = c, gain
				}
			}
			comm[i] = best
			commTot[best] += a.k[i]
			if best != ci {
				moved = true
			}
		}
		if moved {
			merged = true
		} else {
			break
		}
	}
	return comm, merged
}

// aggregate builds the level-up graph: one super-node per community (already
// compacted to 0..count-1 by relabel), with edges summed and intra-community
// weight folded into super self-loops.
func (a *adjacency) aggregate(comm []int, relabel map[int]int, count int) *adjacency {
	super := &adjacency{
		n:    count,
		adj:  make([]map[int]float64, count),
		self: make([]float64, count),
		k:    make([]float64, count),
		twoM: a.twoM, // total weight is invariant under aggregation
	}
	for i := range super.adj {
		super.adj[i] = map[int]float64{}
	}
	for i := 0; i < a.n; i++ {
		ci := relabel[comm[i]]
		super.self[ci] += a.self[i]
		for j, w := range a.adj[i] {
			cj := relabel[comm[j]]
			if ci == cj {
				// Intra-community edge: each unordered pair is seen twice (i->j and
				// j->i), so a half-weight self-loop contribution keeps the total right.
				super.self[ci] += w / 2
			} else {
				super.adj[ci][cj] += w
			}
		}
	}
	for i := 0; i < super.n; i++ {
		sum := 2 * super.self[i]
		for _, w := range super.adj[i] {
			sum += w
		}
		super.k[i] = sum
	}
	return super
}

// modularity returns the modularity Q of a partition (community label per node
// index). Used by tests to prove Louvain is no worse than label propagation.
func (a *adjacency) modularity(comm []int) float64 {
	if a.twoM == 0 {
		return 0
	}
	type acc struct{ in, tot float64 }
	byComm := map[int]*acc{}
	get := func(c int) *acc {
		if byComm[c] == nil {
			byComm[c] = &acc{}
		}
		return byComm[c]
	}
	for i := 0; i < a.n; i++ {
		get(comm[i]).tot += a.k[i]
		get(comm[i]).in += 2 * a.self[i] // A_ii = 2 * self-loop
		for j, w := range a.adj[i] {
			if comm[j] == comm[i] {
				get(comm[i]).in += w // each unordered intra pair counted from both ends
			}
		}
	}
	q := 0.0
	for _, ac := range byComm {
		q += ac.in/a.twoM - (ac.tot/a.twoM)*(ac.tot/a.twoM)
	}
	return q
}

// labelProp is the previous community algorithm (synchronous label propagation),
// kept unexported so a test can show Louvain achieves at least its modularity.
// Each node adopts the most frequent label among its neighbors, ties to the
// lowest label; deterministic in sorted index order.
func (a *adjacency) labelProp(maxRounds int) []int {
	label := make([]int, a.n)
	for i := range label {
		label[i] = i
	}
	for round := 0; round < maxRounds; round++ {
		changed := false
		for i := 0; i < a.n; i++ {
			if len(a.adj[i]) == 0 {
				continue
			}
			counts := map[int]float64{}
			for j, w := range a.adj[i] {
				counts[label[j]] += w
			}
			labels := make([]int, 0, len(counts))
			for l := range counts {
				labels = append(labels, l)
			}
			sort.Ints(labels)
			best, bestCount := label[i], -1.0
			for _, l := range labels {
				if counts[l] > bestCount {
					best, bestCount = l, counts[l]
				}
			}
			if best != label[i] {
				label[i] = best
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return label
}
