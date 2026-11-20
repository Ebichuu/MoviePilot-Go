// Package templating 迁移 V2 通知使用的 Jinja2 模板子集渲染。
package templating

import (
	"fmt"
	"strings"
)

type segmentKind int

const (
	segmentText segmentKind = iota
	segmentExpression
	segmentStatement
)

type segment struct {
	kind  segmentKind
	value string
}

type node interface{ isNode() }

type textNode struct{ text string }

func (textNode) isNode() {}

type exprNode struct{ expression string }

func (exprNode) isNode() {}

type ifBranch struct {
	condition string
	body      []node
}

type ifNode struct{ branches []ifBranch }

func (ifNode) isNode() {}

type forNode struct {
	variables []string
	iterable  string
	body      []node
}

func (forNode) isNode() {}

func parseTemplate(source string) ([]segment, error) {
	segments := make([]segment, 0, 8)
	for index := 0; index < len(source); {
		openExpr := strings.Index(source[index:], "{{")
		openStmt := strings.Index(source[index:], "{%")
		open := -1
		kind := segmentText
		closer := ""
		switch {
		case openExpr >= 0 && (openStmt < 0 || openExpr < openStmt):
			open, kind, closer = index+openExpr, segmentExpression, "}}"
		case openStmt >= 0:
			open, kind, closer = index+openStmt, segmentStatement, "%}"
		}
		if open < 0 {
			segments = append(segments, segment{kind: segmentText, value: source[index:]})
			break
		}
		if open > index {
			segments = append(segments, segment{kind: segmentText, value: source[index:open]})
		}
		bodyStart := open + 2
		closeIndex := strings.Index(source[bodyStart:], closer)
		if closeIndex < 0 {
			return nil, fmt.Errorf("模板标记未闭合")
		}
		segments = append(segments, segment{kind: kind, value: strings.TrimSpace(source[bodyStart : bodyStart+closeIndex])})
		index = bodyStart + closeIndex + len(closer)
	}
	return segments, nil
}

func buildNodes(segments []segment, index *int, stops map[string]bool) ([]node, string, string, error) {
	nodes := make([]node, 0, len(segments))
	for *index < len(segments) {
		current := segments[*index]
		switch current.kind {
		case segmentText:
			nodes = append(nodes, textNode{text: current.value})
			*index++
		case segmentExpression:
			nodes = append(nodes, exprNode{expression: current.value})
			*index++
		case segmentStatement:
			tag, rest := splitTag(current.value)
			if stops != nil && stops[tag] {
				return nodes, tag, rest, nil
			}
			switch tag {
			case "if":
				node, err := parseIf(segments, index, rest)
				if err != nil {
					return nil, "", "", err
				}
				nodes = append(nodes, node)
			case "for":
				node, err := parseFor(segments, index, rest)
				if err != nil {
					return nil, "", "", err
				}
				nodes = append(nodes, node)
			default:
				return nil, "", "", fmt.Errorf("不支持的模板语句 %q", tag)
			}
		}
	}
	return nodes, "", "", nil
}

func parseIf(segments []segment, index *int, condition string) (ifNode, error) {
	node := ifNode{}
	*index++
	currentCondition := condition
	for {
		body, tag, rest, err := buildNodes(segments, index, map[string]bool{"elif": true, "else": true, "endif": true})
		if err != nil {
			return ifNode{}, err
		}
		node.branches = append(node.branches, ifBranch{condition: currentCondition, body: body})
		switch tag {
		case "elif":
			*index++
			currentCondition = rest
		case "else":
			*index++
			elseBody, endTag, _, err := buildNodes(segments, index, map[string]bool{"endif": true})
			if err != nil {
				return ifNode{}, err
			}
			if endTag != "endif" {
				return ifNode{}, fmt.Errorf("if 语句缺少 endif")
			}
			*index++
			node.branches = append(node.branches, ifBranch{body: elseBody})
			return node, nil
		case "endif":
			*index++
			return node, nil
		default:
			return ifNode{}, fmt.Errorf("if 语句缺少 endif")
		}
	}
}

func parseFor(segments []segment, index *int, header string) (forNode, error) {
	parts := strings.SplitN(header, " in ", 2)
	if len(parts) != 2 {
		return forNode{}, fmt.Errorf("for 语句缺少 in")
	}
	variables := make([]string, 0, 2)
	for _, variable := range strings.Split(parts[0], ",") {
		trimmed := strings.TrimSpace(variable)
		if trimmed == "" {
			return forNode{}, fmt.Errorf("for 语句变量无效")
		}
		variables = append(variables, trimmed)
	}
	// V2 模板不使用循环 else；显式拒绝以避免静默行为差异。
	if strings.Contains(parts[1], " recursive") {
		return forNode{}, fmt.Errorf("不支持递归 for 循环")
	}
	*index++
	body, tag, _, err := buildNodes(segments, index, map[string]bool{"endfor": true})
	if err != nil {
		return forNode{}, err
	}
	if tag != "endfor" {
		return forNode{}, fmt.Errorf("for 语句缺少 endfor")
	}
	*index++
	return forNode{variables: variables, iterable: strings.TrimSpace(parts[1]), body: body}, nil
}

func splitTag(value string) (string, string) {
	trimmed := strings.TrimSpace(value)
	if index := strings.IndexAny(trimmed, " \t\n"); index >= 0 {
		return trimmed[:index], strings.TrimSpace(trimmed[index+1:])
	}
	return trimmed, ""
}
