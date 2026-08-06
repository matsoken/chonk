//go:build windows

package cli

import (
	"encoding/json"
	"io"

	"github.com/matsoken/chonk/internal/scan"
)

type jsonNode struct {
	Name      string     `json:"name"`
	Logical   int64      `json:"logical"`
	Allocated int64      `json:"allocated"`
	Dir       bool       `json:"dir,omitempty"`
	Reparse   bool       `json:"reparse,omitempty"`
	Unread    bool       `json:"unreadable,omitempty"`
	Children  []jsonNode `json:"children,omitempty"`
	Truncated int        `json:"truncated,omitempty"` // children omitted by --top
}

type jsonReport struct {
	Root   string `json:"root"`
	Volume struct {
		Total uint64 `json:"total"`
		Used  uint64 `json:"used"`
		Free  uint64 `json:"free"`
	} `json:"volume"`
	Scanned struct {
		Logical   int64 `json:"logical"`
		Allocated int64 `json:"allocated"`
		Files     int   `json:"files"`
		Dirs      int   `json:"dirs"`
	} `json:"scanned"`
	ElapsedMS int64 `json:"elapsed_ms"`
	Warnings  struct {
		Unreadable int `json:"unreadable"`
		Reparse    int `json:"reparse_not_followed"`
		Deduped    int `json:"deduped"`
		Fallback   int `json:"fallback_dirs"`
	} `json:"warnings"`
	Tree jsonNode `json:"tree"`
}

// RenderJSON writes the same data Render prints, honoring --depth and --top so
// the two output modes describe the same slice of the tree.
func RenderJSON(w io.Writer, t *scan.Tree, idx *scan.ChildIndex, rep Report, opt Options) error {
	if opt.Depth < 1 {
		opt.Depth = 1
	}

	var out jsonReport
	out.Root = rep.Root
	out.Volume.Total = rep.Total
	out.Volume.Used = rep.Used
	if rep.Total >= rep.Used {
		out.Volume.Free = rep.Total - rep.Used
	}
	out.Scanned.Logical = t.Entries[0].Size
	out.Scanned.Allocated = t.Entries[0].Alloc
	out.Scanned.Files = rep.Stats.Files
	out.Scanned.Dirs = rep.Stats.Dirs
	out.ElapsedMS = rep.Elapsed.Milliseconds()
	out.Warnings.Unreadable = rep.Stats.Unreadable
	out.Warnings.Reparse = rep.Stats.Reparse
	out.Warnings.Deduped = rep.Deduped
	out.Warnings.Fallback = rep.Fallback

	r := &renderer{t: t, idx: idx, opt: opt}
	out.Tree = r.node(0, 1)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func (r *renderer) node(i uint32, level int) jsonNode {
	e := &r.t.Entries[i]
	n := jsonNode{
		Name:      r.t.Name(i),
		Logical:   e.Size,
		Allocated: e.Alloc,
		Dir:       e.Flags&scan.FlagDir != 0,
		Reparse:   e.Flags&scan.FlagReparse != 0,
		Unread:    e.Flags&scan.FlagUnreadable != 0,
	}
	if level > r.opt.Depth || !n.Dir || n.Reparse {
		return n
	}

	kids := r.idx.Children(i)
	if len(kids) == 0 {
		return n
	}
	sorted := make([]uint32, len(kids))
	copy(sorted, kids)
	r.sortEntries(sorted)
	if r.opt.Top > 0 && len(sorted) > r.opt.Top {
		n.Truncated = len(sorted) - r.opt.Top
		sorted = sorted[:r.opt.Top]
	}
	n.Children = make([]jsonNode, 0, len(sorted))
	for _, c := range sorted {
		n.Children = append(n.Children, r.node(c, level+1))
	}
	return n
}
