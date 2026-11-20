package templating

import "fmt"

type exprTokenKind int

const (
	tokenEOF exprTokenKind = iota
	tokenIdentifier
	tokenNumber
	tokenString
	tokenOperator
	tokenPunctuation
)

type exprToken struct {
	kind  exprTokenKind
	value string
}

func tokenizeExpression(source string) ([]exprToken, error) {
	tokens := make([]exprToken, 0, 16)
	for index := 0; index < len(source); {
		character := source[index]
		switch {
		case character == ' ' || character == '\t' || character == '\n' || character == '\r':
			index++
		case character == '\'' || character == '"':
			end := index + 1
			for end < len(source) && source[end] != character {
				end++
			}
			if end >= len(source) {
				return nil, fmt.Errorf("未闭合的字符串字面量")
			}
			tokens = append(tokens, exprToken{kind: tokenString, value: source[index+1 : end]})
			index = end + 1
		case character >= '0' && character <= '9':
			end := index
			for end < len(source) && (source[end] >= '0' && source[end] <= '9' || source[end] == '.') {
				end++
			}
			tokens = append(tokens, exprToken{kind: tokenNumber, value: source[index:end]})
			index = end
		case isIdentifierStart(character):
			end := index
			for end < len(source) && isIdentifierPart(source[end]) {
				end++
			}
			tokens = append(tokens, exprToken{kind: tokenIdentifier, value: source[index:end]})
			index = end
		case character == '=' || character == '!' || character == '<' || character == '>':
			if index+1 < len(source) && source[index+1] == '=' {
				tokens = append(tokens, exprToken{kind: tokenOperator, value: source[index : index+2]})
				index += 2
			} else if character == '!' {
				return nil, fmt.Errorf("未知的单字符运算符")
			} else {
				tokens = append(tokens, exprToken{kind: tokenOperator, value: source[index : index+1]})
				index++
			}
		case character == '(' || character == ')' || character == '[' || character == ']' ||
			character == ',' || character == '.' || character == '|':
			tokens = append(tokens, exprToken{kind: tokenPunctuation, value: source[index : index+1]})
			index++
		default:
			return nil, fmt.Errorf("表达式包含不支持的字符 %q", string(character))
		}
	}
	tokens = append(tokens, exprToken{kind: tokenEOF})
	return tokens, nil
}

func isIdentifierStart(character byte) bool {
	return character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func isIdentifierPart(character byte) bool {
	return isIdentifierStart(character) || character >= '0' && character <= '9'
}
