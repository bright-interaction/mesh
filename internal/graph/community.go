package graph

import "sort"

// DetectCommunities runs synchronous label propagation deterministically and
// writes a community id onto every node. Each node starts in its own community;
// each round, every node (in sorted-id order) adopts the most frequent community
// among its undirected neighbors, ties broken by lowest community id.
// Communities are renumbered 0..k-1 by their smallest member id so the output is
// stable across runs. This is the Milestone L default; full Louvain is later.
// Returns the number of communities.
func (g *Graph) DetectCommunities(maxRounds int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if maxRounds <= 0 {
		maxRounds = 20
	}

	ids := make([]string, 0, len(g.nodes))
	for id := range g.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	label := make(map[string]int, len(ids))
	for i, id := range ids {
		label[id] = i
	}

	neighbors := func(id string) []string {
		var ns []string
		for _, e := range g.adj[id] {
			ns = append(ns, e.Target)
		}
		for _, e := range g.rev[id] {
			ns = append(ns, e.Source)
		}
		return ns
	}

	for round := 0; round < maxRounds; round++ {
		changed := false
		for _, id := range ids {
			counts := map[int]int{}
			for _, nb := range neighbors(id) {
				counts[label[nb]]++
			}
			if len(counts) == 0 {
				continue
			}
			comms := make([]int, 0, len(counts))
			for c := range counts {
				comms = append(comms, c)
			}
			sort.Ints(comms) // ties resolve to the lowest community id
			best, bestCount := label[id], -1
			for _, c := range comms {
				if counts[c] > bestCount {
					best, bestCount = c, counts[c]
				}
			}
			if best != label[id] {
				label[id] = best
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	// Renumber to contiguous ids ordered by each community's smallest member.
	firstMember := map[int]string{}
	for _, id := range ids { // ids already sorted, so first seen is smallest
		if _, ok := firstMember[label[id]]; !ok {
			firstMember[label[id]] = id
		}
	}
	labels := make([]int, 0, len(firstMember))
	for l := range firstMember {
		labels = append(labels, l)
	}
	sort.Slice(labels, func(i, j int) bool {
		return firstMember[labels[i]] < firstMember[labels[j]]
	})
	remap := make(map[int]int, len(labels))
	for newID, l := range labels {
		remap[l] = newID
	}
	for _, id := range ids {
		g.nodes[id].Community = remap[label[id]]
	}
	return len(labels)
}
