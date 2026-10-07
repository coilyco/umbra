package guardfile

import "sort"

// DescriptionNode is the top-level `description "..."` node every dialect
// parser reads beside `wrap`. Parse sites name it through this constant.
const DescriptionNode = "description"

// siblingNodes lists the top-level nodes umbra's parsers read beside the subject.
var siblingNodes = []string{DescriptionNode}

// SiblingNodes returns, sorted, the top-level names umbra reads beside `wrap` or
// `mcp-upstream`, so a consumer tells an umbra node from an unread one.
func SiblingNodes() []string {
	out := append([]string(nil), siblingNodes...)
	sort.Strings(out)
	return out
}
