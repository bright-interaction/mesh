package index

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
)

// This file grades a vault's ORGANIZATION: are notes typed from the canonical
// vocabulary, connected to the right neighbours, and is every cluster navigable
// via a map. It is deliberately separate from `mesh health` (knowledge lifecycle:
// dead refs, overdue reviews, contradictions) and `mesh lint` (frontmatter
// validity). The three together cover validity, organization, and lifecycle.
//
// It analyses the REAL built graph, so "orphan" / "hub-only" reflect the exact
// edges retrieval walks: both [[wikilinks]] and related: frontmatter, between note
// nodes, ignoring the heading/tag scaffolding edges. The standard it enforces is
// Hive/ORGANIZATION.md.

// canonicalTypes is the eight-type vocabulary from the structure standard.
var canonicalTypes = map[string]bool{
	"entity": true, "concept": true, "map": true,
	"decision": true, "gotcha": true, "post-mortem": true,
	"note": true, "status": true,
}

// tier0Structure mirrors retrieve.tier0Types: the institutional-memory tier.
var tier0Structure = map[string]bool{"decision": true, "gotcha": true, "post-mortem": true}

// godDegreeStruct mirrors retrieve.godDegree: graph expansion skips hubs above this
// degree, so a note that links only to hubs gains nothing from the graph signal.
const godDegreeStruct = 24

// StructureFinding is one organization problem on one note (or cluster).
type StructureFinding struct {
	Severity string // high | med | low
	Kind     string // untyped | unknown-type | orphan | weak-link | hub-only | mapless-cluster
	Path     string
	Detail   string
}

// ClusterMember is one note in a cluster, for building a map from.
type ClusterMember struct {
	Title  string
	Path   string
	Type   string
	Degree int
}

// ClusterInfo is a cluster that needs a map: its members, most-connected first,
// so the map can lead with the anchors.
type ClusterInfo struct {
	ID      int
	Size    int
	Members []ClusterMember
}

// StructureReport is the vault's organization grade plus the itemized findings.
type StructureReport struct {
	Notes           int
	Score           int // 0-100
	Grade           string
	ByType          map[string]int
	Tier0           int
	Orphans         int
	Clusters        int
	Mapless         int
	Findings        []StructureFinding
	MaplessClusters []ClusterInfo // members of each mapless cluster, to author its map
}

// AnalyzeStructure grades the organization of a parsed vault against its built
// graph (run DetectCommunities first so cluster checks work). Pure; no I/O.
func AnalyzeStructure(g *graph.Graph, parsed []*ParsedNote) StructureReport {
	rep := StructureReport{ByType: map[string]int{}}
	deg := map[string]int{}
	for _, n := range g.Nodes() {
		deg[n.ID] = n.Degree
	}

	clusterSize := map[int]int{}
	clusterHasMap := map[int]bool{}
	clusterMembers := map[int][]ClusterMember{}
	flagged := map[string]bool{} // note ids with a high/med finding (for the score)

	flag := func(id, sev, kind, path, detail string) {
		rep.Findings = append(rep.Findings, StructureFinding{sev, kind, path, detail})
		if sev == "high" || sev == "med" {
			flagged[id] = true
		}
	}

	for _, pn := range parsed {
		if isMetaDoc(pn.Path) {
			continue // README/CLAUDE/ORGANIZATION are operational docs, not graph knowledge
		}
		nodeID := "note:" + effectiveID(pn)
		node, ok := g.Node(nodeID)
		if !ok {
			continue
		}
		rep.Notes++
		t := strings.TrimSpace(string(pn.FM.Type))
		rep.ByType[t]++
		if tier0Structure[t] {
			rep.Tier0++
		}
		clusterSize[node.Community]++
		clusterMembers[node.Community] = append(clusterMembers[node.Community], ClusterMember{
			Title: strings.TrimSpace(pn.FM.Title), Path: pn.Path, Type: t, Degree: node.Degree,
		})
		if t == "map" {
			clusterHasMap[node.Community] = true
		}

		switch {
		case t == "":
			flag(nodeID, "high", "untyped", pn.Path, "no type; declare one of the 8 canonical types")
		case !canonicalTypes[t]:
			flag(nodeID, "high", "unknown-type", pn.Path, "type '"+t+"' is not canonical (see ORGANIZATION.md)")
		}

		refs, hubOnly := noteRefs(g, nodeID, deg)
		switch {
		case refs == 0:
			rep.Orphans++
			flag(nodeID, "high", "orphan", pn.Path, "no links to other notes; nothing reaches it, it reaches nothing")
		case refs == 1:
			flag(nodeID, "low", "weak-link", pn.Path, "only one link; connect it to a couple of siblings")
		case hubOnly:
			flag(nodeID, "med", "hub-only", pn.Path, "links only to hub notes (index/log/big maps); expansion skips hubs, link the specific note")
		}
	}

	for c, size := range clusterSize {
		rep.Clusters++
		if size >= 6 && !clusterHasMap[c] {
			rep.Mapless++
			rep.Findings = append(rep.Findings, StructureFinding{"med", "mapless-cluster", "",
				fmt.Sprintf("cluster #%d has %d notes but no map; add a maps/ front-door for it", c, size)})
			mem := append([]ClusterMember(nil), clusterMembers[c]...)
			sort.SliceStable(mem, func(i, j int) bool { return mem[i].Degree > mem[j].Degree })
			rep.MaplessClusters = append(rep.MaplessClusters, ClusterInfo{ID: c, Size: size, Members: mem})
		}
	}
	sort.SliceStable(rep.MaplessClusters, func(i, j int) bool { return rep.MaplessClusters[i].Size > rep.MaplessClusters[j].Size })

	rep.Score, rep.Grade = scoreStructure(rep.Notes, len(flagged), rep.Mapless)
	sort.SliceStable(rep.Findings, func(i, j int) bool {
		if sevRank(rep.Findings[i].Severity) != sevRank(rep.Findings[j].Severity) {
			return sevRank(rep.Findings[i].Severity) < sevRank(rep.Findings[j].Severity)
		}
		return rep.Findings[i].Path < rep.Findings[j].Path
	})
	return rep
}

// noteRefs returns how many OTHER note nodes this note references (in or out, via
// [[links]] or related:), and whether every one of them is a hub (degree >
// godDegreeStruct). Heading/tag scaffolding edges are ignored: only note<->note
// "references" edges are knowledge connections.
func noteRefs(g *graph.Graph, id string, deg map[string]int) (count int, hubOnly bool) {
	seen := map[string]bool{}
	hubOnly = true
	consider := func(other string) {
		if !strings.HasPrefix(other, "note:") || other == id || seen[other] {
			return
		}
		seen[other] = true
		count++
		if deg[other] <= godDegreeStruct {
			hubOnly = false
		}
	}
	for _, e := range g.Neighbors(id) {
		if e.Relation == "references" {
			consider(e.Target)
		}
	}
	for _, e := range g.RefsTo(id) {
		if e.Relation == "references" {
			consider(e.Source)
		}
	}
	if count == 0 {
		hubOnly = false
	}
	return count, hubOnly
}

// scoreStructure: the share of notes with no high/med problem, minus a small
// penalty per mapless cluster. Intuitive ("X% of notes are well-structured") and
// honest. Grade bands are the usual A-F.
func scoreStructure(notes, flagged, mapless int) (int, string) {
	if notes == 0 {
		return 100, "A"
	}
	clean := float64(notes-flagged) / float64(notes) * 100.0
	clean -= float64(mapless) * 2.0
	score := int(clean + 0.5)
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	switch {
	case score >= 90:
		return score, "A"
	case score >= 80:
		return score, "B"
	case score >= 70:
		return score, "C"
	case score >= 60:
		return score, "D"
	default:
		return score, "F"
	}
}

// isMetaDoc reports whether a path is an operational doc (the vault's readme /
// agent instructions / structure standard) rather than a knowledge note, so it is
// excluded from the organization grade.
func isMetaDoc(path string) bool {
	switch strings.ToUpper(path[strings.LastIndexByte(path, '/')+1:]) {
	case "README.MD", "CLAUDE.MD", "ORGANIZATION.MD":
		return true
	}
	return false
}

func sevRank(s string) int {
	switch s {
	case "high":
		return 0
	case "med":
		return 1
	default:
		return 2
	}
}
