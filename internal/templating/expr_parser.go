package templating

import (
	"fmt"
	"strconv"
)

type exprParser struct {
	tokens  []exprToken
	index   int
	context map[string]any
}

func (p *exprParser) peek() exprToken {
	return p.tokens[p.index]
}

func (p *exprParser) next() exprToken {
	token := p.tokens[p.index]
	if token.kind != tokenEOF {
		p.index++
	}
	return token
}

func (p *exprParser) matchOperator(values ...string) bool {
	token := p.peek()
	if token.kind != tokenOperator {
		return false
	}
	for _, value := range values {
		if token.value == value {
			p.index++
			return true
		}
	}
	return false
}

func (p *exprParser) matchIdentifier(value string) bool {
	token := p.peek()
	if token.kind == tokenIdentifier && token.value == value {
		p.index++
		return true
	}
	return false
}

func (p *exprParser) parse() (any, error) {
	value, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokenEOF {
		return nil, fmt.Errorf("表达式存在多余内容")
	}
	return value, nil
}

func (p *exprParser) parseOr() (any, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.matchIdentifier("or") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = truthy(left) || truthy(right)
	}
	return left, nil
}

func (p *exprParser) parseAnd() (any, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.matchIdentifier("and") {
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = truthy(left) && truthy(right)
	}
	return left, nil
}

func (p *exprParser) parseNot() (any, error) {
	if p.matchIdentifier("not") {
		value, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return !truthy(value), nil
	}
	return p.parseComparison()
}

func (p *exprParser) parseComparison() (any, error) {
	left, err := p.parseFilter()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.matchOperator("=="):
			right, err := p.parseFilter()
			if err != nil {
				return nil, err
			}
			left = equalsValue(left, right)
		case p.matchOperator("!="):
			right, err := p.parseFilter()
			if err != nil {
				return nil, err
			}
			left = !equalsValue(left, right)
		case p.matchOperator("<", "<=", ">", ">="):
			operator := p.tokens[p.index-1].value
			right, err := p.parseFilter()
			if err != nil {
				return nil, err
			}
			comparison, compareErr := compareValues(operator, left, right)
			if compareErr != nil {
				return nil, compareErr
			}
			left = comparison
		case p.matchIdentifier("in"):
			right, err := p.parseFilter()
			if err != nil {
				return nil, err
			}
			left = containsValue(right, left)
		default:
			return left, nil
		}
	}
}

func (p *exprParser) parseFilter() (any, error) {
	value, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokenPunctuation && p.peek().value == "|" {
		p.index++
		name := p.next()
		if name.kind != tokenIdentifier {
			return nil, fmt.Errorf("过滤器缺少名称")
		}
		arguments := []any{}
		if p.peek().kind == tokenPunctuation && p.peek().value == "(" {
			p.index++
			for p.peek().kind != tokenPunctuation || p.peek().value != ")" {
				if p.peek().kind == tokenEOF {
					return nil, fmt.Errorf("过滤器参数缺少右括号")
				}
				argument, err := p.parseOr()
				if err != nil {
					return nil, err
				}
				arguments = append(arguments, argument)
				if p.peek().kind == tokenPunctuation && p.peek().value == "," {
					p.index++
					continue
				}
				break
			}
			if p.peek().kind != tokenPunctuation || p.peek().value != ")" {
				return nil, fmt.Errorf("过滤器参数缺少右括号")
			}
			p.index++
		}
		filtered, filterErr := applyFilter(name.value, value, arguments)
		if filterErr != nil {
			return nil, filterErr
		}
		value = filtered
	}
	return value, nil
}

func (p *exprParser) parseUnary() (any, error) {
	if p.peek().kind == tokenPunctuation && p.peek().value == "(" {
		p.index++
		value, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokenPunctuation || p.peek().value != ")" {
			return nil, fmt.Errorf("表达式缺少右括号")
		}
		p.index++
		return value, nil
	}
	return p.parsePrimary()
}

func (p *exprParser) parsePrimary() (any, error) {
	token := p.next()
	switch token.kind {
	case tokenString:
		return token.value, nil
	case tokenNumber:
		if integer, err := strconv.ParseInt(token.value, 10, 64); err == nil {
			return integer, nil
		}
		number, err := strconv.ParseFloat(token.value, 64)
		if err != nil {
			return nil, fmt.Errorf("无效数字字面量")
		}
		return number, nil
	case tokenIdentifier:
		switch token.value {
		case "true", "True":
			return true, nil
		case "false", "False":
			return false, nil
		case "none", "None", "null":
			return nil, nil
		}
		value, ok := p.context[token.value]
		if !ok {
			return nil, nil
		}
		return p.resolveAttributes(value)
	default:
		return nil, fmt.Errorf("表达式缺少操作数")
	}
}

func (p *exprParser) resolveAttributes(value any) (any, error) {
	for p.peek().kind == tokenPunctuation && p.peek().value == "." {
		p.index++
		name := p.next()
		if name.kind != tokenIdentifier {
			return nil, fmt.Errorf("属性访问缺少名称")
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil, nil
		}
		value = object[name.value]
	}
	return value, nil
}
