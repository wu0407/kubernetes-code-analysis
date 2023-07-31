/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package jsonpath

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const eof = -1

const (
	leftDelim  = "{"
	rightDelim = "}"
)

type Parser struct {
	Name  string
	Root  *ListNode
	input string
	pos   int
	start int
	width int
}

var (
	ErrSyntax        = errors.New("invalid syntax")
	dictKeyRex       = regexp.MustCompile(`^'([^']*)'$`)
	// （-?[\d]*）：负号+数字
	// （:-?[\d]*)：冒号+负号+数字
	// （:-?[\d]*)?：冒号+负号+数字，可选
	sliceOperatorRex = regexp.MustCompile(`^(-?[\d]*)(:-?[\d]*)?(:-?[\d]*)?$`)
)

// Parse parsed the given text and return a node Parser.
// If an error is encountered, parsing stops and an empty
// Parser is returned with the error
func Parse(name, text string) (*Parser, error) {
	p := NewParser(name)
	err := p.Parse(text)
	if err != nil {
		p = nil
	}
	return p, err
}

func NewParser(name string) *Parser {
	return &Parser{
		Name: name,
	}
}

// parseAction parsed the expression inside delimiter
// text左右两边加上分隔符（'{'+{text}+'}'）后，再解析
func parseAction(name, text string) (*Parser, error) {
	// text左右两边加上分隔符（'{'+{text}+'}'）后，再解析
	p, err := Parse(name, fmt.Sprintf("%s%s%s", leftDelim, text, rightDelim))
	// when error happens, p will be nil, so we need to return here
	if err != nil {
		return p, err
	}
	// 确保p.Root替换为p.Root.Nodes[0]
	p.Root = p.Root.Nodes[0].(*ListNode)
	return p, nil
}

func (p *Parser) Parse(text string) error {
	p.input = text
	// ListNode{NodeType: NodeList}
	p.Root = newList()
	p.pos = 0
	return p.parseText(p.Root)
}

// consumeText return the parsed text since last cosumeText
// 解析开头到当前位置的内容，返回p.start到p.pos（不包含p.pos）的内容
// 并将p.start开始位置设置为当前p.pos位置，向左移动
func (p *Parser) consumeText() string {
	value := p.input[p.start:p.pos]
	p.start = p.pos
	return value
}

// next returns the next rune in the input.
// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下下一个字符位置（p.pos += p.width），返回下一个字符的rune
// 否则，设置p.width宽度为0，返回eof
func (p *Parser) next() rune {
	// 已经到结尾了
	if p.pos >= len(p.input) {
		// 重置宽度为0
		p.width = 0
		return eof
	}
	// 还未到结尾，获取下一个的字符
	r, w := utf8.DecodeRuneInString(p.input[p.pos:])
	// 设置宽度为这个的字符长度
	p.width = w
	// 移动p.pos到下一个字符的位置
	p.pos += p.width
	return r
}

// peek returns but does not consume the next rune in the input.
// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下一个字符位置（p.pos不变），返回下一个字符的rune
// 否则，设置宽度为0，返回eof，p.pos不变
func (p *Parser) peek() rune {
	// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下下一个字符位置（p.pos += p.width），返回下一个字符的rune
	// 否则，设置宽度为0，返回eof
	r := p.next()
	// p.pos回到下一个字符的位置
	p.backup()
	return r
}

// backup steps back one rune. Can only be called once per call of next.
// p.pos回到上一个字符的位置
func (p *Parser) backup() {
	p.pos -= p.width
}

func (p *Parser) parseText(cur *ListNode) error {
	for {
		// 当前位置到最后包含"{"前缀
		if strings.HasPrefix(p.input[p.pos:], leftDelim) {
			// p.consumeText()
			// 解析开头到当前位置的内容，返回解析的内容
			// 并将开始位置设置为当前位置，向左移动
			// newText
			// 返回&TextNode{NodeType: NodeText, Text: p.consumeText()}
			// 当前位置不处于开始位置，说明左分隔符前面有内容，需要先将内容添加到cur中
			if p.pos > p.start {
				cur.append(newText(p.consumeText()))
			}
			// 解析左分隔符
			return p.parseLeftDelim(cur)
		}
		// 没有找到到左分隔符"{"

		// 如果到结尾了，结束
		if p.next() == eof {
			break
		}
	}
	// Correctly reached EOF.
	// 当前位置到最后不包含"{"前缀，p.pos == len(p.input)，且p.pos > p.start（还有未解析字符）
	if p.pos > p.start {
		// 读取p.start到p.pos（不包含p.pos）的内容
		// &TextNode{NodeType: NodeText, Text: p.consumeText()} append到cur的nodes
		cur.append(newText(p.consumeText()))
	}
	return nil
}

// parseLeftDelim scans the left delimiter, which is known to be present.
func (p *Parser) parseLeftDelim(cur *ListNode) error {
	// p.pos加1，跳过左分隔符
	p.pos += len(leftDelim)
	// 解析开头到当前位置的内容，返回解析的内容（这里忽略"{"）
	// 并将开始位置设置为当前位置，向左移动
	// p.start移动到"{"后面一个位置
	p.consumeText()
	newNode := newList()
	// 将&ListNode{NodeType: NodeList} append到cur.Nodes
	cur.append(newNode)
	// 将cur指向newNode
	cur = newNode
	// 继续解析剩余字符串
	return p.parseInsideAction(cur)
}

func (p *Parser) parseInsideAction(cur *ListNode) error {
	prefixMap := map[string]func(*ListNode) error{
		// "}"
		rightDelim: p.parseRightDelim,
		"[?(":      p.parseFilter,
		"..":       p.parseRecursive,
	}
	// 前缀匹配"}"、"[?("、".."，则调用对应的方法解析对应的内容
	for prefix, parseFunc := range prefixMap {
		if strings.HasPrefix(p.input[p.pos:], prefix) {
			return parseFunc(cur)
		}
	}

	// 前缀不匹配"}"、"[?("、".."

	// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下下一个字符位置（p.pos += p.width），返回下一个字符的rune
	// 否则，设置p.width宽度为0，返回eof
	switch r := p.next(); {
	// 如果是结尾或者换行符'\r'、'\n'，返回错误
	case r == eof || isEndOfLine(r):
		return fmt.Errorf("unclosed action")
	// 如果是空格，跳过空格，将p.start设值为p.pos，即p.start移动到空格后面一个位置
	case r == ' ':
		// 将p.start设值为p.pos，即p.start移动到空格后面一个位置
		p.consumeText()
	// 如果是"@"或者"$"，即当前对象或根对象，直接跳过
	case r == '@' || r == '$': //the current object, just pass it
		p.consumeText()
	// 如果是'['（后面没有跟"?("），则解析数组
	case r == '[':
		return p.parseArray(cur)
	// 如果是单引号或双引号，解析字符串
	case r == '"' || r == '\'':
		return p.parseQuote(cur, r)
	case r == '.':
		// 如果是"."，则解析当前对象
		return p.parseField(cur)
	// 如果是'+','-',数字，则解析数字
	case r == '+' || r == '-' || unicode.IsDigit(r):
		// p.pos回到上一个字符的位置（+','-',数字的位置）
		p.backup()
		return p.parseNumber(cur)
	// 如果是下划线或字母，则解析标识符
	case isAlphaNumeric(r):
		// p.pos回到上一个字符的位置（下划线或字母的位置）
		p.backup()
		return p.parseIdentifier(cur)
	default:
		return fmt.Errorf("unrecognized character in action: %#U", r)
	}
	return p.parseInsideAction(cur)
}

// parseRightDelim scans the right delimiter, which is known to be present.
// p.pos和p.start都加1，跳过"}"，继续解析p.Root（即重新解析左边剩下的内容）
func (p *Parser) parseRightDelim(cur *ListNode) error {
	// p.pos加1，跳过右分隔符
	p.pos += len(rightDelim)
	// 解析开头到当前位置的内容，返回解析的内容（这里忽略"}"）
	// 并将开始位置设置为当前位置，向左移动
	// p.start移动到"}"后面一个位置
	p.consumeText()
	// 重新解析p.Root
	return p.parseText(p.Root)
}

// parseIdentifier scans build-in keywords, like "range" "end"
func (p *Parser) parseIdentifier(cur *ListNode) error {
	var r rune
	// 一直向前，直到遇到终止符，p.pos为终止符的位置
	for {
		r = p.next()
		// 如果r为' '、'\t'、'\r'、'\n'、eof、'.'、','、'['、']'、'$'、'@'、'{'、'}'，则返回true
		// 否则返回false
		if isTerminator(r) {
			// p.pos回到上一个字符的位置（终止符的位置）
			p.backup()
			break
		}
	}
	// 读取字符，p.start设置为p.pos，即p.start移动到终止符位置
	value := p.consumeText()

	// 如果是bool值，解析bool值，然后将&BoolNode{NodeType: NodeBool, Value: v} append到cur.Nodes
	if isBool(value) {
		v, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("can not parse bool '%s': %s", value, err.Error())
		}

		// cur的nodes append &BoolNode{NodeType: NodeBool, Value: v}
		cur.append(newBool(v))
	} else {
		// cur的nodes append &IdentifierNode{NodeType: NodeIdentifier, Name: value}
		cur.append(newIdentifier(value))
	}

	// 继续解析剩余的内容
	return p.parseInsideAction(cur)
}

// parseRecursive scans the recursive descent operator ..
func (p *Parser) parseRecursive(cur *ListNode) error {
	// 前面已经有一个recursive，不允许个recursive
	// 前面的node是NodeRecursive，这个节点的也是NodeRecursive，返回错误
	if lastIndex := len(cur.Nodes) - 1; lastIndex >= 0 && cur.Nodes[lastIndex].Type() == NodeRecursive {
		return fmt.Errorf("invalid multiple recursive descent")
	}
	// p.pos加2，即到".."后面位置
	p.pos += len("..")
	// 解析开头到当前位置的内容，返回解析的内容（这里忽略".."）
	// 并将p.start开始位置设置为p.pos当前位置，向左移动
	p.consumeText()
	// cur.Nodes append &RecursiveNode{NodeType: NodeRecursive}
	cur.append(newRecursive())
	// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下一个字符位置（p.pos不变），返回下一个字符的rune
	// 否则，设置宽度为0，返回eof，p.pos不变
	if r := p.peek(); isAlphaNumeric(r) {
		// 下一个字符是下划线、字母、数字
		return p.parseField(cur)
	}
	return p.parseInsideAction(cur)
}

// parseNumber scans number
func (p *Parser) parseNumber(cur *ListNode) error {
	// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下一个字符位置（p.pos不变），返回下一个字符的rune
	// 否则，设置宽度为0，返回eof，p.pos不变
	r := p.peek()
	// 如果是'+'或者'-'，则跳过
	if r == '+' || r == '-' {
		// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下一个字符位置（'+'或者'-'后面的字符）
		// 否则，设置宽度为0，返回eof，p.pos不变
		p.next()
	}
	// 循环开始时p.pos都为第一个数字的位置

	// 一直向前，直到下一个字符不是'.'且不是数字
	// p.pos为最后一个数字后面的（不是'.'且不是数字）位置
	for {
		// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下一个字符位置（p.pos += p.width），返回下一个字符的rune
		// 否则，设置p.width宽度为0，返回eof
		r = p.next()
		// 下一个字符不是'.'且不是数字，回退一个字符，跳出循环
		// p.pos为最后一个数字后面的位置
		if r != '.' && !unicode.IsDigit(r) {
			p.backup()
			break
		}
	}
	// 读取数字，p.start设置为p.pos（不是'.'且不是数字）位置
	value := p.consumeText()
	i, err := strconv.Atoi(value)
	if err == nil {
		// 如果是整数，cur.Nodes append &IntNode{NodeType: NodeInt, Value: i}
		cur.append(newInt(i))
		// 继续解析剩下字符
		return p.parseInsideAction(cur)
	}
	// 如果是浮点数，cur.Nodes append &FloatNode{NodeType: NodeFloat, Value: d}
	d, err := strconv.ParseFloat(value, 64)
	if err == nil {
		cur.append(newFloat(d))
		return p.parseInsideAction(cur)
	}
	// 否则，返回错误
	return fmt.Errorf("cannot parse number %s", value)
}

// parseArray scans array index selection
func (p *Parser) parseArray(cur *ListNode) error {
Loop:
	// 一直向前，直到找到"]"，即数组结束。p.pos为"]"后面的的位置
	for {
		// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下下一个字符位置（p.pos += p.width），返回下一个字符的rune
		// 否则，设置p.width宽度为0，返回eof
		switch p.next() {
		case eof, '\n':
			return fmt.Errorf("unterminated array")
		case ']':
			break Loop
		}
	}
	// 读取从"["到"]"之间的内容，即数组的内容。将p.start开始位置设置为当前p.pos位置（"]"后面的的位置）
	text := p.consumeText()
	// 移除前后的中括号，只保留"["到"]"之间的内容
	text = text[1 : len(text)-1]
	if text == "*" {
		text = ":"
	}

	//union operator
	// 按逗号分割，如果有多个，即有多个数组，如[1,2,3]，则创建多个union节点，将union节点添加到cur.Nodes中
	strs := strings.Split(text, ",")
	// 超过一个item，即有多个数组
	if len(strs) > 1 {
		union := []*ListNode{}
		for _, str := range strs {
			// 解析数组，创建union节点
			// str移除前后空格后，左右两边加上分隔符（'{['+{str}+']}'）后，再解析。比如[1,2,3]，解析后为{[1]},{[2]},{[3]}
			parser, err := parseAction("union", fmt.Sprintf("[%s]", strings.Trim(str, " ")))
			if err != nil {
				return err
			}
			union = append(union, parser.Root)
		}
		// 将union节点&UnionNode{NodeType: NodeUnion, Nodes: union}添加到cur.Nodes中
		cur.append(newUnion(union))
		// 解析剩下的内容
		return p.parseInsideAction(cur)
	}

	// dict key
	// 只有一个item，即只有一个数组
	// 单引号包起来的字符串，如['a']，则创建一个dict节点，将dict节点添加到cur.Nodes中
	value := dictKeyRex.FindStringSubmatch(text)
	if value != nil {
		// value[1]前面添加'.'号后，左右两边加上分隔符（'{'+{str}+'}'）后，再解析。比如['a']，解析为{'.a'}
		parser, err := parseAction("arraydict", fmt.Sprintf(".%s", value[1]))
		if err != nil {
			return err
		}
		// 将dict节点&FieldNode{NodeType: NodeField, Value: value}添加到cur.Nodes中
		for _, node := range parser.Root.Nodes {
			cur.append(node)
		}
		// 解析剩下的内容
		return p.parseInsideAction(cur)
	}

	//slice operator
	// [start:end]或[start:]或[:end]或[start:end:step]，则创建一个slice节点，将slice节点添加到cur.Nodes中
	value = sliceOperatorRex.FindStringSubmatch(text)
	if value == nil {
		return fmt.Errorf("invalid array index %s", text)
	}
	// value[1]前面添加'.'号后，左右两边加上分隔符（'{'+{str}+'}'）后，再解析。比如[1:2]，解析为{'.1:2'}
	// value为移除第一个完全正则匹配的内容
	value = value[1:]
	params := [3]ParamsEntry{}
	for i := 0; i < 3; i++ {
		// 如果value[i]不为空，即有值，则移除第一个字符，即":"，并将params[i].Known设置为true，params[i].Value设置为value[i]的值
		if value[i] != "" {
			// 不是第一部分，移除第一个字符，即":"
			if i > 0 {
				value[i] = value[i][1:]
			}
			// 不是第一部份，且value[i]为空，即":"，则params[i].Known设置为false，params[i].Value设置为0
			if i > 0 && value[i] == "" {
				params[i].Known = false
			} else {
				// 否则，将value[i]转为int类型，如果转换成功，则params[i].Known设置为true，params[i].Value设置为value[i]的值
				var err error
				params[i].Known = true
				params[i].Value, err = strconv.Atoi(value[i])
				if err != nil {
					return fmt.Errorf("array index %s is not a number", value[i])
				}
			}
		} else {
			// 如果value[i]为空
			// 如果是第二部分为空，说明是类似这种"-3"。则params[i].Known设置为true，params[i].Value设置为params[0].Value+1，params[i].Derived设置为true
			if i == 1 {
				params[i].Known = true
				params[i].Value = params[0].Value + 1
				params[i].Derived = true
			} else {
				// 否则，params[i].Known设置为false，params[i].Value设置为0
				params[i].Known = false
				params[i].Value = 0
			}
		}
	}
	// &ArrayNode{
	// 	NodeType: NodeArray,
	// 	Params:   params,
	// }
	// 将slice节点&ArrayNode{NodeType: NodeArray, Params: params}添加到cur.Nodes中
	cur.append(newArray(params))
	// 解析剩下的内容
	return p.parseInsideAction(cur)
}

// parseFilter scans filter inside array selection
func (p *Parser) parseFilter(cur *ListNode) error {
	// p.pos加上3（"[?("的长度）
	p.pos += len("[?(")
	// p.start设置为p.pos，就是把"[?("去掉
	p.consumeText()
	begin := false
	end := false
	var pair rune

Loop:
	// 先遍历所有字符，检查引号是否成对，如果成对，设置begin为true，end为true。如果不成对，设置begin为true，end为false。
	// 检查是否Filter是否结束（括号成对）
	for {
		// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下下一个字符位置（p.pos += p.width），返回所下一个字符的rune
		// 否则，设置宽度为0，返回eof
		r := p.next()
		switch r {
		case eof, '\n':
			// 返回没有终结的报错
			return fmt.Errorf("unterminated filter")
		// 如果是双引号或者单引号
		case '"', '\'':
			// 如果是第一次遇到引号（左半边引号），设置begin为true，保存引号类型，继续下一次循环
			if begin == false {
				//save the paired rune
				begin = true
				pair = r
				continue
			}
			//only add when met paired rune
			// 如果是第二次遇到引号（右半边引号且没有对引号进行转义），设置end为true，继续下一次循环
			if p.input[p.pos-2] != '\\' && r == pair {
				end = true
			}
		// 如果是右半边括号，且有成对的引号出现过，则终止循环，说明已经找到结束的右半边括号
		case ')':
			//in rightParser below quotes only appear zero or once
			//and must be paired at the beginning and end
			if begin == end {
				break Loop
			}
		}
	}
	// ')'括号后面不是']'，则返回错误
	if p.next() != ']' {
		return fmt.Errorf("unclosed array expect ]")
	}
	reg := regexp.MustCompile(`^([^!<>=]+)([!<>=]+)(.+?)$`)
	// 返回左标记'[?('后里面到')]'的内容，同时将p.start设置为p.pos（都为']'后面的位置)
	text := p.consumeText()
	// 移除')]'，即括号里面的内容
	text = text[:len(text)-2]
	value := reg.FindStringSubmatch(text)
	// 内容不是条件过滤，即不是{key}{operator}{value}的形式，即条件为对象存在
	if value == nil {
		// text左右两边加上分隔符（'{'+{text}+'}'）后，再解析
		parser, err := parseAction("text", text)
		if err != nil {
			return err
		}
		// &FilterNode{
		// 	NodeType: NodeFilter,
		// 	Left:     parser.Root,
		// 	Right:    &ListNode{NodeType: NodeList},
		// 	Operator: "exists",
		// }
		// cur.nodes append FilterNode
		cur.append(newFilter(parser.Root, newList(), "exists"))
	} else {
		// value[1]左右两边加上分隔符（'{'+{text}+'}'）后，再解析
		leftParser, err := parseAction("left", value[1])
		if err != nil {
			return err
		}
		// value[3]左右两边加上分隔符（'{'+{text}+'}'）后，再解析
		rightParser, err := parseAction("right", value[3])
		if err != nil {
			return err
		}
		// &FilterNode{
		// 	NodeType: NodeFilter,
		// 	Left:     leftParser.Root,
		// 	Right:    rightParser.Root,
		// 	Operator: value[2],
		// }
		// cur.nodes append FilterNode
		cur.append(newFilter(leftParser.Root, rightParser.Root, value[2]))
	}
	// 继续解析剩余字符串
	return p.parseInsideAction(cur)
}

// parseQuote unquotes string inside double or single quote
func (p *Parser) parseQuote(cur *ListNode, end rune) error {
Loop:
	// 遍历所有字符，直到遇到结束的引号
	for {
		// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下下一个字符位置（p.pos += p.width），返回所下一个字符的rune
		// 否则，设置宽度为0，返回eof
		switch p.next() {
		case eof, '\n':
			return fmt.Errorf("unterminated quoted string")
		case end:
			//if it's not escape break the Loop
			// 如果引号前面不是转义字符，终止循环
			if p.input[p.pos-2] != '\\' {
				break Loop
			}
		}
	}
	// 获取引号里面的内容（包括前后引号）
	// p.start设置为p.pos（都为右半边引号后面的位置）
	value := p.consumeText()
	// 移除前后引号
	s, err := UnquoteExtend(value)
	if err != nil {
		return fmt.Errorf("unquote string %s error %v", value, err)
	}
	// cur.nodes append &TextNode{NodeType: NodeText, Text: s}
	cur.append(newText(s))
	// 解析剩余字符串
	return p.parseInsideAction(cur)
}

// parseField scans a field until a terminator
// 这时的p.pos为'.'后面的位置
func (p *Parser) parseField(cur *ListNode) error {
	// 设置p.start为p.pos（'.'后面的位置）
	p.consumeText()
	// p.advance()
	// 如果下一个字符为'\'，则继续获取下一个字符，设置p.width宽度为'\'后面（下下一个）的字符长度，p.pos为'\'后面字符后面字符（下下下一个字符）位置（p.pos += p.width），返回true
	// 如果下一个字符为' '、'\t'、'\r'、'\n'、eof、'.'、','、'['、']'、'$'、'@'、'{'、'}'，p.pos不变，返回false
	// 其他字符，则获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下下一个字符位置（p.pos += p.width），返回true
	//
	// 一直向前，直到找到下一个字符为' '、'\t'、'\r'、'\n'、eof、'.'、','、'['、']'、'$'、'@'、'{'、'}'
	// 这个时候p.pos为最后一个字符（' '、'\t'、'\r'、'\n'、eof、'.'、','、'['、']'、'$'、'@'、'{'、'}'）的位置
	for p.advance() {
	}
	// 解析开头到当前位置的内容，返回p.start到p.pos（不包含p.pos）的内容
	// 并将p.start开始位置设置为当前p.pos位置，向左移动
	value := p.consumeText()
	if value == "*" {
		// cur.nodes append &WildcardNode{NodeType: NodeWildcard}
		cur.append(newWildcard())
	} else {
		// cur.nodes append &FieldNode{NodeType: NodeField, Value: value（去除'\'）}
		cur.append(newField(strings.Replace(value, "\\", "", -1)))
	}
	// 继续解析剩余字符串
	return p.parseInsideAction(cur)
}

// advance scans until next non-escaped terminator
// 如果下一个字符为'\'，则继续获取下一个字符，设置p.width宽度为'\'后面（下下一个）的字符长度，p.pos为'\'后面字符后面字符（下下下一个字符）位置（p.pos += p.width），返回true
// 如果下一个字符为' '、'\t'、'\r'、'\n'、eof、'.'、','、'['、']'、'$'、'@'、'{'、'}'，p.pos不变，返回false
// 其他字符，则获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下下一个字符位置（p.pos += p.width），返回true
func (p *Parser) advance() bool {
	// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下下一个字符位置（p.pos += p.width），返回下一个字符的rune
	// 否则，设置p.width宽度为0，返回eof
	r := p.next()
	// 如果r为'\'，则获取下一个字符，设置p.width宽度为下一个的字符长度，p.pos为下下一个字符位置（p.pos += p.width）
	if r == '\\' {
		// 如果还未到结尾，获取下一个的字符，设置p.width宽度为下一个的字符长度，p.pos为下下一个字符位置（p.pos += p.width），返回下一个字符的rune
		// 否则，设置p.width宽度为0，返回eof
		p.next()
	// 如果r为' '、'\t'、'\r'、'\n'、eof、'.'、','、'['、']'、'$'、'@'、'{'、'}'，则返回true
	} else if isTerminator(r) {
		// p.post回到上一个字符位置，因为上面执行了p.next()，所以要还原
		p.backup()
		return false
	}
	return true
}

// isTerminator reports whether the input is at valid termination character to appear after an identifier.
// 如果r为' '、'\t'、'\r'、'\n'、eof、'.'、','、'['、']'、'$'、'@'、'{'、'}'，则返回true
// 否则返回false
func isTerminator(r rune) bool {
	// 如果r为' '、'\t'、'\r'、'\n'，则返回true
	if isSpace(r) || isEndOfLine(r) {
		return true
	}
	switch r {
	case eof, '.', ',', '[', ']', '$', '@', '{', '}':
		return true
	}
	return false
}

// isSpace reports whether r is a space character.
func isSpace(r rune) bool {
	return r == ' ' || r == '\t'
}

// isEndOfLine reports whether r is an end-of-line character.
func isEndOfLine(r rune) bool {
	return r == '\r' || r == '\n'
}

// isAlphaNumeric reports whether r is an alphabetic, digit, or underscore.
// 下划线、字母、数字
func isAlphaNumeric(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// isBool reports whether s is a boolean value.
func isBool(s string) bool {
	return s == "true" || s == "false"
}

// UnquoteExtend is almost same as strconv.Unquote(), but it support parse single quotes as a string
func UnquoteExtend(s string) (string, error) {
	n := len(s)
	if n < 2 {
		return "", ErrSyntax
	}
	quote := s[0]
	// 前后两个引号必须一样
	if quote != s[n-1] {
		return "", ErrSyntax
	}
	// 去掉前后引号
	s = s[1 : n-1]

	// 不为单引号或双引号，返回错误
	if quote != '"' && quote != '\'' {
		return "", ErrSyntax
	}

	// Is it trivial?  Avoid allocation.
	// 如果字符串中不包含'\'和quote，直接返回
	if !contains(s, '\\') && !contains(s, quote) {
		return s, nil
	}

	// 引号里面的内容包含'\'或引号

	var runeTmp [utf8.UTFMax]byte
	buf := make([]byte, 0, 3*len(s)/2) // Try to avoid more allocations.
	// 通过循环，将字符串中的'\'和引号替换为真实字符，将结果保存到buf中
	for len(s) > 0 {
		// 解析第一个字符，返回解析后的字符、是否为多字节字符、解析后剩余的字符串、错误
		c, multibyte, ss, err := strconv.UnquoteChar(s, quote)
		if err != nil {
			return "", err
		}
		s = ss
		// 一个字节的字符或者不是多字节字符，直接追加到buf中
		if c < utf8.RuneSelf || !multibyte {
			buf = append(buf, byte(c))
		} else {
			// 多字节字符，将字符转换为utf8编码，追加到buf中
			n := utf8.EncodeRune(runeTmp[:], c)
			buf = append(buf, runeTmp[:n]...)
		}
	}
	return string(buf), nil
}

func contains(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}
