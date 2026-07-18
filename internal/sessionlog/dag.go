package sessionlog

// dagNode is a node in the conversation DAG.
type dagNode struct {
	uuid      string
	parentID  string // parentUuid (empty string = root)
	lineIndex int    // 0-based position in JSONL file
	entry     *Entry
}

// DagResult is the result of resolving a session's DAG to its active
// conversation branch.
type DagResult struct {
	// ActiveBranch is the messages on the active branch, root to tip.
	ActiveBranch []*Entry

	// OrphanedToolUseIDs contains tool_use IDs on the active branch
	// that have no matching tool_result anywhere in the session.
	OrphanedToolUseIDs map[string]bool

	// HasBranches is true if the session has multiple tips (forks).
	HasBranches bool

	// CompactionCount is the number of compact_boundary entries on
	// the active branch.
	CompactionCount int
}

// BuildDag resolves a slice of entries into the active conversation branch.
//
// Algorithm:
//  1. Build maps: uuid → node, parentUuid → children
//  2. Find tips: entries with no children
//  3. Select active tip: most recent timestamp, tiebreaker longest branch
//  4. Walk tip to root via parentUuid chain (following logicalParentUuid
//     across compact boundaries)
//  5. Collect all tool_result IDs from entire session
//  6. Find orphaned tool_use blocks on active branch
func BuildDag(entries []*Entry) *DagResult {
	nodeMap := make(map[string]*dagNode)
	childrenMap := make(map[string][]string) // parentUuid → child uuids

	// Build node and children maps.
	for i, e := range entries {
		if e.UUID == "" {
			continue // skip entries without UUID (file-history-snapshot, etc.)
		}
		node := &dagNode{
			uuid:      e.UUID,
			parentID:  e.ParentUUID,
			lineIndex: i,
			entry:     e,
		}
		nodeMap[e.UUID] = node
		childrenMap[e.ParentUUID] = append(childrenMap[e.ParentUUID], e.UUID)
	}

	// Find tips (nodes with no children).
	type tipInfo struct {
		node   *dagNode
		length int
	}
	var tips []tipInfo
	for _, node := range nodeMap {
		children := childrenMap[node.uuid]
		if len(children) == 0 {
			length := walkBranchLength(node.uuid, nodeMap)
			tips = append(tips, tipInfo{node: node, length: length})
		}
	}

	if len(tips) == 0 {
		return &DagResult{}
	}

	// Select active tip: latest timestamp wins, then longest branch,
	// then latest lineIndex.
	best := tips[0]
	for _, t := range tips[1:] {
		bestTs := best.node.entry.Timestamp
		currentTs := t.node.entry.Timestamp
		switch {
		case currentTs.After(bestTs):
			best = t
		case bestTs.After(currentTs):
			// keep best
		default:
			// Same timestamp — prefer longer branch, then later line.
			if t.length > best.length ||
				(t.length == best.length && t.node.lineIndex > best.node.lineIndex) {
				best = t
			}
		}
	}

	// Walk from tip to root.
	var activeBranch []*Entry
	activeBranchUUIDs := make(map[string]bool)
	visited := make(map[string]bool)
	compactionCount := 0

	current := best.node
	for current != nil && !visited[current.uuid] {
		visited[current.uuid] = true
		activeBranch = append(activeBranch, current.entry)
		activeBranchUUIDs[current.uuid] = true

		if current.entry.IsCompactBoundary() {
			compactionCount++
		}

		// Determine next: parentUuid, or logicalParentUuid for compact boundaries.
		nextID := current.parentID
		if nextID == "" && current.entry.LogicalParentUUID != "" {
			nextID = current.entry.LogicalParentUUID
		}

		if nextID == "" {
			break
		}

		next, ok := nodeMap[nextID]
		if !ok && current.entry.LogicalParentUUID != "" {
			// logicalParentUuid references a message not in this file.
			// Fallback: find the node with highest lineIndex before current.
			next = findFallbackParent(current.lineIndex, nodeMap, visited)
		} else if !ok {
			break
		}
		current = next
	}

	// Reverse to get root → tip order.
	for i, j := 0, len(activeBranch)-1; i < j; i, j = i+1, j-1 {
		activeBranch[i], activeBranch[j] = activeBranch[j], activeBranch[i]
	}

	// Collect all tool_result IDs from entire session (not just active branch).
	allToolResultIDs := collectAllToolResultIDs(entries)

	// Find orphaned tool_use blocks on active branch.
	orphaned := findOrphanedToolUses(activeBranch, allToolResultIDs)

	return &DagResult{
		ActiveBranch:       activeBranch,
		OrphanedToolUseIDs: orphaned,
		HasBranches:        len(tips) > 1,
		CompactionCount:    compactionCount,
	}
}

// conversationTypes are message types that count toward branch length.
var conversationTypes = map[string]bool{
	"user":      true,
	"assistant": true,
}

// walkBranchLength counts conversation messages (user/assistant) from
// tip to root. Used for branch selection tiebreaking.
func walkBranchLength(tipUUID string, nodeMap map[string]*dagNode) int {
	count := 0
	visited := make(map[string]bool)
	currentID := tipUUID

	for currentID != "" && !visited[currentID] {
		visited[currentID] = true
		node, ok := nodeMap[currentID]
		if !ok {
			break
		}
		if conversationTypes[node.entry.Type] {
			count++
		}
		nextID := node.parentID
		if nextID == "" && node.entry.LogicalParentUUID != "" {
			nextID = node.entry.LogicalParentUUID
		}
		if nextID != "" && nodeMap[nextID] == nil && node.entry.LogicalParentUUID != "" {
			fb := findFallbackParent(node.lineIndex, nodeMap, visited)
			if fb != nil {
				currentID = fb.uuid
			} else {
				break
			}
		} else {
			currentID = nextID
		}
	}
	return count
}

// findFallbackParent returns the node with the highest lineIndex before
// beforeIdx that hasn't been visited. Used when a compact_boundary's
// logicalParentUuid doesn't exist in this session file.
func findFallbackParent(beforeIdx int, nodeMap map[string]*dagNode, visited map[string]bool) *dagNode {
	var best *dagNode
	for _, n := range nodeMap {
		if n.lineIndex >= beforeIdx || visited[n.uuid] {
			continue
		}
		if best == nil || n.lineIndex > best.lineIndex {
			best = n
		}
	}
	return best
}

// collectAllToolResultIDs scans all entries for tool_result blocks and
// returns a set of their tool_use_id references. This scans the entire
// session (not just active branch) because parallel tool calls can
// produce results on sibling branches.
func collectAllToolResultIDs(entries []*Entry) map[string]bool {
	ids := make(map[string]bool)
	for _, e := range entries {
		// Top-level tool_result entries carry the tool_use_id directly.
		if e.ToolUseID != "" && (e.Type == "result" || e.Type == "tool_result") {
			ids[e.ToolUseID] = true
		}
		// Also check nested content blocks.
		blocks := e.ContentBlocks()
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				ids[b.ToolUseID] = true
			}
		}
	}
	return ids
}

// findOrphanedToolUses returns tool_use IDs on the active branch that
// have no matching tool_result anywhere in the session.
func findOrphanedToolUses(activeBranch []*Entry, allToolResultIDs map[string]bool) map[string]bool {
	toolUseIDs := make(map[string]bool)
	for _, e := range activeBranch {
		blocks := e.ContentBlocks()
		for _, b := range blocks {
			if b.Type == "tool_use" && b.ID != "" {
				toolUseIDs[b.ID] = true
			}
		}
	}

	orphaned := make(map[string]bool)
	for id := range toolUseIDs {
		if !allToolResultIDs[id] {
			orphaned[id] = true
		}
	}
	if len(orphaned) == 0 {
		return nil
	}
	return orphaned
}
