package expansion

import (
	"bytes"
)

const (
	operator        = '$'
	referenceOpener = '('
	referenceCloser = ')'
)

// syntaxWrap returns the input string wrapped by the expansion syntax.
func syntaxWrap(input string) string {
	return string(operator) + string(referenceOpener) + input + string(referenceCloser)
}

// MappingFuncFor returns a mapping function for use with Expand that
// implements the expansion semantics defined in the expansion spec; it
// returns the input string wrapped in the expansion syntax if no mapping
// for the input is found.
// 返回函数，在context列表里每个map查找input，找到就返回，否则返回"$({input})"，比如"$(var)"
func MappingFuncFor(context ...map[string]string) func(string) string {
	return func(input string) string {
		for _, vars := range context {
			val, ok := vars[input]
			if ok {
				return val
			}
		}

		// 返回"$({input})"，比如"$(var)"
		return syntaxWrap(input)
	}
}

// Expand replaces variable references in the input string according to
// the expansion spec using the given mapping function to resolve the
// values of variables.
// 处理类似"$(var)"格式，执行mapping替换var，和"$$"格式转义
func Expand(input string, mapping func(string) string) string {
	var buf bytes.Buffer
	checkpoint := 0
	for cursor := 0; cursor < len(input); cursor++ {
		// 匹配"$"且不是最后一个字符
		if input[cursor] == operator && cursor+1 < len(input) {
			// Copy the portion of the input string since the last
			// checkpoint into the buffer
			// 上次匹配"$"字符进行处理后第一个未处理字符到再次匹配"$"之间的字符存到buf
			// 比如input为"$(asd)hjk$zxc"，上一次处理了"$(asd)"，这次再次匹配"$"，则append保存"hjk"到buf
			buf.WriteString(input[checkpoint:cursor])

			// Attempt to read the variable name as defined by the
			// syntax from the input string
			// 对剩余部分进行解析
			// 解析完字符，是否为替换符，未处理剩余字符串的起始位置（在input[cursor+1:]里的位置，相对于0）相当于在input里当前（算上input[cursor]）已经处理字符数
			read, isVar, advance := tryReadVariableName(input[cursor+1:])

			// 如果是替换符
			if isVar {
				// We were able to read a variable name correctly;
				// apply the mapping to the variable name and copy the
				// bytes into the buffer
				// 执行mapping进行查找read替换值，append写入到buf
				buf.WriteString(mapping(read))
			} else {
				// Not a variable name; copy the read bytes into the buffer
				// 已经解析的字符append写入到buf
				buf.WriteString(read)
			}

			// Advance the cursor in the input string to account for
			// bytes consumed to read the variable name expression
			// 移动到已经处理的字符位置
			cursor += advance

			// Advance the checkpoint in the input string
			// checkpoint移动到未处理字符位置
			checkpoint = cursor + 1
		}
	}

	// Return the buffer and any remaining unwritten bytes in the
	// input string.
	// 从buf中字符（替换、转义、"$"+{字符}会放在buf中）和剩下未处理字符（不匹配"$"）
	return buf.String() + input[checkpoint:]
}

// tryReadVariableName attempts to read a variable name from the input
// string and returns the content read from the input, whether that content
// represents a variable name to perform mapping on, and the number of bytes
// consumed in the input string.
//
// The input string is assumed not to contain the initial operator.
// 返回第一个参数，解析完的字符串（input是"$asd"，转义的返回"$"；input是"(asd)",替换符返回需要替换的变量名"asd"；input是"(asd"或"asd"，返回"$"和第一个字符串）
// 返回第二个参数，true为替换符，false为值
// 返回第三个参数，未处理剩余字符串的起始位置
func tryReadVariableName(input string) (string, bool, int) {
	// 这里假设传入的input是已经去除"$"的后半部分，比如原来字符是"asd$$key"，传入给tryReadVariableName的input为"$key"
	switch input[0] {
	// 第一个字符是"$"，直接返回"$"和false和1
	// 说明这个是escape语法，传给tryReadVariableName之前的字符串是"$$asd"，代表"$asd"
	case operator:
		// Escaped operator; return it.
		return input[0:1], false, 1
	// 第一个字符是"("
	case referenceOpener:
		// Scan to expression closer
		// 查找")"，如果找到，则返回"("和")"中间的字符串，true，剩余部分（遍历到的下一个）位置i + 1
		for i := 1; i < len(input); i++ {
			if input[i] == referenceCloser {
				return input[1:i], true, i + 1
			}
		}

		// Incomplete reference; return it.
		// 没有找到")"，比如input为"(dsd"。则返回"$("，false，1
		return string(operator) + string(referenceOpener), false, 1
	// 第一个字符即不是"$"，也不是"("
	default:
		// Not the beginning of an expression, ie, an operator
		// that doesn't begin an expression.  Return the operator
		// and the first rune in the string.
		// 比如input是"asd"或"a$(sd)"
		// 返回"$"和第一个字符串"${input[0]}"，false，1
		return (string(operator) + string(input[0])), false, 1
	}
}
