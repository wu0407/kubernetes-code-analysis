// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package json is forked from the Go standard library to enable us to find the
// field of a struct that a given JSON key maps to.
package json

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	patchStrategyTagKey = "patchStrategy"
	patchMergeKeyTagKey = "patchMergeKey"
)

// Finds the patchStrategy and patchMergeKey struct tag fields on a given
// struct field given the struct type and the JSON name of the field.
// It returns field type, a slice of patch strategies, merge key and error.
// TODO: fix the returned errors to be introspectable.
// t的类型或底层类型不是struct，返回错误
// 从t类型的对应的结构体里查找json tag名字为jsonField字段
// 获取字段tag里面的"patchStrategy"的值，并按逗号进行分隔
// 获取字段tag里面"patchMergeKey"的值
// 返回字段的类型reflect.Type，"patchStrategy"的分隔后的值，"patchMergeKey"的值
func LookupPatchMetadataForStruct(t reflect.Type, jsonField string) (
	elemType reflect.Type, patchStrategies []string, patchMergeKey string, e error) {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		e = fmt.Errorf("merging an object in json but data type is not struct, instead is: %s",
			t.Kind().String())
		return
	}
	jf := []byte(jsonField)
	// Find the field that the JSON library would use.
	var f *field
	// 返回t类型对应所有json导出字段
	fields := cachedTypeFields(t)
	for i := range fields {
		ff := &fields[i]
		// 找到jsonField相关的字段，则终止循环
		if bytes.Equal(ff.nameBytes, jf) {
			f = ff
			break
		}
		// Do case-insensitive comparison.
		// 进行转换后，nameBytes与jsonField的bytes相同，找到jsonField相关的字段
		if f == nil && ff.equalFold(ff.nameBytes, jf) {
			f = ff
		}
	}
	// 找到jsonField相关的字段
	if f != nil {
		// Find the reflect.Value of the most preferential struct field.
		// t下面第一层级字段reflect.StructField（jsonField相关的字段）
		tjf := t.Field(f.index[0])
		// we must navigate down all the anonymously included structs in the chain
		// 最后一层字段reflect.StructField
		for i := 1; i < len(f.index); i++ {
			tjf = tjf.Type.Field(f.index[i])
		}
		// 获取字段tag里面的"patchStrategy"的值
		patchStrategy := tjf.Tag.Get(patchStrategyTagKey)
		// 获取字段tag里面"patchMergeKey"的值
		patchMergeKey = tjf.Tag.Get(patchMergeKeyTagKey)
		patchStrategies = strings.Split(patchStrategy, ",")
		elemType = tjf.Type
		return
	}
	e = fmt.Errorf("unable to find api field in struct %s for the json field %q", t.Name(), jsonField)
	return
}

// A field represents a single field found in a struct.
type field struct {
	name      string
	nameBytes []byte                 // []byte(name)
	equalFold func(s, t []byte) bool // bytes.EqualFold or equivalent

	tag bool
	// index is the sequence of indexes from the containing type fields to this field.
	// it is a slice because anonymous structs will need multiple navigation steps to correctly
	// resolve the proper fields
	index     []int
	typ       reflect.Type
	omitEmpty bool
	quoted    bool
}

func (f field) String() string {
	return fmt.Sprintf("{name: %s, type: %v, tag: %v, index: %v, omitEmpty: %v, quoted: %v}", f.name, f.typ, f.tag, f.index, f.omitEmpty, f.quoted)
}

// f.name转成[]byte赋值为f.nameBytes
// 根据f.nameBytes设置f.equalFold
func fillField(f field) field {
	f.nameBytes = []byte(f.name)
	// s中包含字符大于大于128，说明是non-ASCII字符，返回bytes.EqualFold
	// s中包含'K'或'S'，或'k'或's'，返回equalFoldRight
	// s中包含非字母字符，返回asciiEqualFold
	// 剩下情况，返回simpleLetterEqualFold
	f.equalFold = foldFunc(f.nameBytes)
	return f
}

// byName sorts field by name, breaking ties with depth,
// then breaking ties with "name came from json tag", then
// breaking ties with index sequence.
type byName []field

func (x byName) Len() int { return len(x) }

func (x byName) Swap(i, j int) { x[i], x[j] = x[j], x[i] }

// 先按照name字段进行排序
// 再按照index的长度（字段的层级）进行排序，层级越少的排在前面
// 再按照tag（是否有json tag）排序，有的排在前面（都有则顺序不变）
// 按照index字段（保存所处的位置）里相同位置值比较小的位置排前面
// 再比较层级（index的长度），层级越少的排在前面
func (x byName) Less(i, j int) bool {
	if x[i].name != x[j].name {
		return x[i].name < x[j].name
	}
	if len(x[i].index) != len(x[j].index) {
		return len(x[i].index) < len(x[j].index)
	}
	if x[i].tag != x[j].tag {
		return x[i].tag
	}
	return byIndex(x).Less(i, j)
}

// byIndex sorts field by index sequence.
type byIndex []field

func (x byIndex) Len() int { return len(x) }

func (x byIndex) Swap(i, j int) { x[i], x[j] = x[j], x[i] }

// 按照index字段（保存所处的位置）里相同位置值比较小的位置排前面
// 再比较层级，层级越少的排在前面
func (x byIndex) Less(i, j int) bool {
	for k, xik := range x[i].index {
		// 层级比x[j]更深，所以x[j]排前面
		if k >= len(x[j].index) {
			return false
		}
		// 在len(x[j].index)长度内，比较相同层级的位置，小的排在前面
		if xik != x[j].index[k] {
			return xik < x[j].index[k]
		}
	}
	// x[i].index长度小于等于x[j].index长度
	// 按照x[i].index里所有位置都一样，比较层级越少的排在前面，层级相同的后面的排前面
	return len(x[i].index) < len(x[j].index)
}

// typeFields returns a list of fields that JSON should recognize for the given type.
// The algorithm is breadth-first search over the set of structs to include - the top struct
// and then any reachable anonymous structs.
// 返回t下面所有json导出的字段
func typeFields(t reflect.Type) []field {
	// Anonymous fields to explore at the current level and the next.
	current := []field{}
	next := []field{{typ: t}}

	// Count of queued names for current level and the next.
	count := map[reflect.Type]int{}
	nextCount := map[reflect.Type]int{}

	// Types already visited at an earlier level.
	visited := map[reflect.Type]bool{}

	// Fields found.
	var fields []field

	// 只分析一级字段和匿名结构体
	for len(next) > 0 {
		current, next = next, current[:0]
		count, nextCount = nextCount, map[reflect.Type]int{}

		for _, f := range current {
			// 字段类型已经遍历过，就跳过
			if visited[f.typ] {
				continue
			}
			// 否则就记录
			visited[f.typ] = true

			// Scan f.typ for fields to include.
			for i := 0; i < f.typ.NumField(); i++ {
				// 字段的类型
				sf := f.typ.Field(i)
				// 为空代表这个字段是未导出字段
				// 未导出字段，则跳过
				if sf.PkgPath != "" { // unexported
					continue
				}
				// 获取字段tag里的"json"字段
				tag := sf.Tag.Get("json")
				// json里未导出字段跳过
				if tag == "-" {
					continue
				}
				// tag里有逗号，返回第一个","前面字符串和后面字符串
				// 没有","，则返回tag和空的tagOptions
				name, opts := parseTag(tag)
				// name为空，则返回false
				// name里包含不是字母且不是数字，且不是"!#$%&()*+-./:<=>?@[]^_{|}~ "返回false
				// 否则返回true
				if !isValidTag(name) {
					name = ""
				}
				index := make([]int, len(f.index)+1)
				// 拷贝父层级
				copy(index, f.index)
				// index包含了f.index，且最后添加了当前字段index
				// 这样做是为了添加匿名结构体里的位置（第一个位第一级位置，第二个为匿名结构体位置，第三匿名结构体里匿名结构体位置，依此类推）
				index[len(f.index)] = i

				ft := sf.Type
				// 如果为指针类型，则ft为元素类型
				if ft.Name() == "" && ft.Kind() == reflect.Ptr {
					// Follow pointer.
					ft = ft.Elem()
				}

				// Record found field and index sequence.
				// name不为空（json tag里的name部分合法），或不为匿名，或字段不为struct
				// 继续下一个字段
				if name != "" || !sf.Anonymous || ft.Kind() != reflect.Struct {
					tagged := name != ""
					// 没有定义json tag，则name为字段名
					if name == "" {
						name = sf.Name
					}
					// f.name转成[]byte赋值为f.nameBytes
					// 根据f.nameBytes设置f.equalFold
					fields = append(fields, fillField(field{
						name:      name,
						tag:       tagged,
						index:     index,
						typ:       ft,
						// o为空返回false
						// 对o进行逗号分隔，然后进行分段遍历，查找是否包含optionName。找到返回true，否则返回false
						omitEmpty: opts.Contains("omitempty"),
						quoted:    opts.Contains("string"),
					}))
					// 有相同的类型（只有两个相同的匿名结构会大于1，这个不会发生，golang不允许）（匿名结构体第一次计数会先加1，这里不会大于1）
					if count[f.typ] > 1 {
						// If there were multiple instances, add a second,
						// so that the annihilation code will see a duplicate.
						// It only cares about the distinction between 1 or 2,
						// so don't bother generating any more copies.
						// 再添加一个重复的field，用于下面的dominantField处理
						fields = append(fields, fields[len(fields)-1])
					}
					continue
				}

				// Record new anonymous struct to explore in next round.
				// 匿名结构体
				// 计数加1
				nextCount[ft]++
				// 第一次遇见这个类型，添加field到next
				if nextCount[ft] == 1 {
					next = append(next, fillField(field{name: ft.Name(), index: index, typ: ft}))
				}
			}
		}
	}

	// 先按照name字段进行排序
	// 再按照index的长度（字段的层级）进行排序，层级越少的排在前面
	// 再按照tag（是否有json tag）排序，有的排在前面（都有则顺序不变）
	// 按照index字段（保存所处的位置）里相同位置值比较小的位置排前面
	// 再比较层级（index的长度），层级越少的排在前面
	sort.Sort(byName(fields))

	// Delete all fields that are hidden by the Go rules for embedded fields,
	// except that fields with JSON tags are promoted.

	// The fields are sorted in primary order of name, secondary order
	// of field index length. Loop over names; for each name, delete
	// hidden fields by choosing the one dominant field that survives.
	out := fields[:0]
	for advance, i := 0, 0; i < len(fields); i += advance {
		// One iteration per name.
		// Find the sequence of fields with the name of this first field.
		fi := fields[i]
		name := fi.name
		// 找到i后面第一个name不一样的元素fj
		for advance = 1; i+advance < len(fields); advance++ {
			fj := fields[i+advance]
			if fj.name != name {
				break
			}
		}
		// 后面一个元素name就不一样（这个name的只有元素fi），则将fi append到out
		if advance == 1 { // Only one field with this name
			out = append(out, fi)
			continue
		}
		// 从存在name一样的元素里
		// 找到第一个最少层级设置了json tag的field，如果找到设置了json tag的field，返回这个field，true
		// 没有找到设置json tag，且存在多个同一层级（最少层级）的field，返回field{}, false
		// 同一层级（最少层级）里已经有field的设置了json tag，但是还找到同样设置了相同的json tag，说明冲突了，返回field{}, false
		// 没有找到设置json tag，只有一个（最少层级）field，返回这个fields[0], true
		dominant, ok := dominantField(fields[i : i+advance])
		// 找到设置了json tag的field，或只有一个最少层级没有设置json tag
		if ok {
			out = append(out, dominant)
		}
	}

	fields = out
	// 按照index字段（保存所处的位置）里相同位置值比较小的位置排前面
	// 再比较层级（index的长度），层级越少的排在前面
	sort.Sort(byIndex(fields))

	return fields
}

// dominantField looks through the fields, all of which are known to
// have the same name, to find the single field that dominates the
// others using Go's embedding rules, modified by the presence of
// JSON tags. If there are multiple top-level fields, the boolean
// will be false: This condition is an error in Go and we skip all
// the fields.
// 找到第一个最少层级设置了json tag的field，如果找到设置了json tag的field，返回这个field，true
// 没有找到设置json tag，且存在多个同一层级（最少层级）的field，返回field{}, false
// 同一层级（最少层级）里已经有field的设置了json tag，但是还找到同样设置了相同的json tag，说明冲突了，返回field{}, false
// 没有找到设置json tag，只有一个（最少层级）field，返回这个fields[0], true
func dominantField(fields []field) (field, bool) {
	// The fields are sorted in increasing index-length order. The winner
	// must therefore be one with the shortest index length. Drop all
	// longer entries, which is easy: just truncate the slice.
	// 只取第一个（匿名结构体里的字段有重复的field），层级最少
	length := len(fields[0].index)
	tagged := -1 // Index of first tagged field.
	// 从最少层级field里，找到第一个设置了json tag的field
	for i, f := range fields {
		// 有当前field的层级大于最少的层级
		if len(f.index) > length {
			// fields为最少层级的哪一些
			fields = fields[:i]
			break
		}
		if f.tag {
			// 同一层级里已经有field的设置了json tag，但是还找到同样设置了相同的json tag，说明冲突了，返回field{}, false
			if tagged >= 0 {
				// Multiple tagged fields at the same level: conflict.
				// Return no field.
				return field{}, false
			}
			tagged = i
		}
	}
	// 找到设置了json tag的field，返回这个field，true
	if tagged >= 0 {
		return fields[tagged], true
	}
	// All remaining fields have the same length. If there's more than one,
	// we have a conflict (two fields named "X" at the same level) and we
	// return no field.
	// 最少层级没有找到设置json tag，且存在多个同一层级（最少层级）的field，返回field{}, false
	if len(fields) > 1 {
		return field{}, false
	}
	// 最少层级里没有找到设置json tag，只有一个（最少层级）field，返回这个fields[0], true
	return fields[0], true
}

var fieldCache struct {
	sync.RWMutex
	m map[reflect.Type][]field
}

// cachedTypeFields is like typeFields but uses a cache to avoid repeated work.
// 返回t类型对应所有json导出字段
func cachedTypeFields(t reflect.Type) []field {
	fieldCache.RLock()
	f := fieldCache.m[t]
	fieldCache.RUnlock()
	// 从缓存中找到，返回类型对应的字段[]field
	if f != nil {
		return f
	}

	// Compute fields without lock.
	// Might duplicate effort but won't hold other computations back.
	// 返回t下面所有json导出的字段
	f = typeFields(t)
	if f == nil {
		f = []field{}
	}

	// 保存f到fieldCache.m[t]
	fieldCache.Lock()
	if fieldCache.m == nil {
		fieldCache.m = map[reflect.Type][]field{}
	}
	fieldCache.m[t] = f
	fieldCache.Unlock()
	return f
}

// s为空，则返回false
// s里包含不是字母且不是数字，且不是"!#$%&()*+-./:<=>?@[]^_{|}~ "返回false
// 否则返回true
func isValidTag(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:<=>?@[]^_{|}~ ", c):
			// Backslash and quote chars are reserved, but
			// otherwise any punctuation chars are allowed
			// in a tag name.
		default:
			// 不是字母且不是数字，返回false
			if !unicode.IsLetter(c) && !unicode.IsDigit(c) {
				return false
			}
		}
	}
	return true
}

const (
	// 这里上角标是取反，得到数字是223
	caseMask     = ^byte(0x20) // Mask to ignore case in ASCII.
	kelvin       = '\u212a'
	smallLongEss = '\u017f'
)

// foldFunc returns one of four different case folding equivalence
// functions, from most general (and slow) to fastest:
//
// 1) bytes.EqualFold, if the key s contains any non-ASCII UTF-8
// 2) equalFoldRight, if s contains special folding ASCII ('k', 'K', 's', 'S')
// 3) asciiEqualFold, no special, but includes non-letters (including _)
// 4) simpleLetterEqualFold, no specials, no non-letters.
//
// The letters S and K are special because they map to 3 runes, not just 2:
//  * S maps to s and to U+017F 'ſ' Latin small letter long s
//  * k maps to K and to U+212A 'K' Kelvin sign
// See http://play.golang.org/p/tTxjOc0OGo
//
// The returned function is specialized for matching against s and
// should only be given s. It's not curried for performance reasons.
// s中包含字符大于大于128，说明是non-ASCII字符，返回bytes.EqualFold
// s中包含'K'或'S'，或'k'或's'，返回equalFoldRight
// s中包含非字母字符，返回asciiEqualFold
// 剩下情况，返回simpleLetterEqualFold
func foldFunc(s []byte) func(s, t []byte) bool {
	nonLetter := false
	special := false // special letter
	for _, b := range s {
		// 大于128，说明是non-ASCII字符
		if b >= utf8.RuneSelf {
			return bytes.EqualFold
		}
		// 忽略大小写，转成大写
		upper := b & caseMask
		// 非字母字符
		if upper < 'A' || upper > 'Z' {
			nonLetter = true
		} else if upper == 'K' || upper == 'S' {
			// See above for why these letters are special.
			special = true
		}
	}
	// 包含'K'或'S'，或'k'或's'
	if special {
		return equalFoldRight
	}
	// 非字母字符
	if nonLetter {
		return asciiEqualFold
	}
	return simpleLetterEqualFold
}

// equalFoldRight is a specialization of bytes.EqualFold when s is
// known to be all ASCII (including punctuation), but contains an 's',
// 'S', 'k', or 'K', requiring a Unicode fold on the bytes in t.
// See comments on foldFunc.
func equalFoldRight(s, t []byte) bool {
	for _, sb := range s {
		if len(t) == 0 {
			return false
		}
		tb := t[0]
		// tb是ascii码字符
		// 如果相等则跳过这个字符，继续下一个字符。
		// 如果不相等，且将sb转成大写且在A-Z范围内，且与tb转成大写相等，则跳过这个字符，继续下一个字符。否则返回false
		if tb < utf8.RuneSelf {
			// sb跟t的第一个byte不相等
			if sb != tb {
				// 将sb转成大写
				sbUpper := sb & caseMask
				// sb在A-Z范围里
				if 'A' <= sbUpper && sbUpper <= 'Z' {
					// 都转成大写后，还是不相等，返回false
					if sbUpper != tb&caseMask {
						return false
					}
				} else {
					// sb不在A-Z范围里，返回false
					return false
				}
			}
			// sb跟t的第一个byte相等，或转成大写后相等
			// 跳过t里的这个字符
			t = t[1:]
			continue
		}
		// tb不是ascii码字符
		// sb必须是s, S, k, or K，t转成rune后是smallLongEss或kelvin，否则返回false
		// sb is ASCII and t is not. t must be either kelvin
		// sign or long s; sb must be s, S, k, or K.
		// 将t解析成rune
		tr, size := utf8.DecodeRune(t)
		switch sb {
		// sb是's'或'S'，t解析成rune后不是smallLongEss，返回false
		case 's', 'S':
			if tr != smallLongEss {
				return false
			}
		// sb是'k'或'K'，t解析成rune后不是kelvin，返回false
		case 'k', 'K':
			if tr != kelvin {
				return false
			}
		// sb是其他情况，返回false
		default:
			return false
		}
		// 跳过解析成rune的byte
		t = t[size:]

	}
	// 对比完后，t还有字符还未比较，则返回false
	if len(t) > 0 {
		return false
	}
	return true
}

// asciiEqualFold is a specialization of bytes.EqualFold for use when
// s is all ASCII (but may contain non-letters) and contains no
// special-folding letters.
// See comments on foldFunc.
// 都是ascii码，但是包含不是字母
func asciiEqualFold(s, t []byte) bool {
	// 长度不相等返回false
	if len(s) != len(t) {
		return false
	}
	for i, sb := range s {
		tb := t[i]
		// 相同位置相等，则继续下一个字符
		if sb == tb {
			continue
		}
		// sb是字母
		if ('a' <= sb && sb <= 'z') || ('A' <= sb && sb <= 'Z') {
			// 都转成大写后不相等，返回false
			if sb&caseMask != tb&caseMask {
				return false
			}
		} else {
			// sb不是字母，返回false
			return false
		}
	}
	return true
}

// simpleLetterEqualFold is a specialization of bytes.EqualFold for
// use when s is all ASCII letters (no underscores, etc) and also
// doesn't contain 'k', 'K', 's', or 'S'.
// See comments on foldFunc.
// 字母且不包含'k', 'K', 's', or 'S'
// 长度不相等返回false
// 遍历每个byte，都转成大写后不相等，返回false
// 否则，返回true
func simpleLetterEqualFold(s, t []byte) bool {
	// 长度不相等返回false
	if len(s) != len(t) {
		return false
	}
	for i, b := range s {
		// 都转成大写后不相等，返回false
		if b&caseMask != t[i]&caseMask {
			return false
		}
	}
	return true
}

// tagOptions is the string following a comma in a struct field's "json"
// tag, or the empty string. It does not include the leading comma.
type tagOptions string

// parseTag splits a struct field's json tag into its name and
// comma-separated options.
// tag里有逗号，返回第一个","前面字符串和后面字符串
// 没有","，则返回tag和空的tagOptions
func parseTag(tag string) (string, tagOptions) {
	// tag里有逗号，返回第一个","前面字符串和后面字符串
	// json tag类似这样'json:"matchExpressions,omitempty"'
	if idx := strings.Index(tag, ","); idx != -1 {
		return tag[:idx], tagOptions(tag[idx+1:])
	}
	// 没有","，则返回tag和空的tagOptions
	return tag, tagOptions("")
}

// Contains reports whether a comma-separated list of options
// contains a particular substr flag. substr must be surrounded by a
// string boundary or commas.
// o为空返回false
// 对o进行逗号分隔，然后进行分段遍历，查找是否包含optionName。找到返回true，否则返回false
func (o tagOptions) Contains(optionName string) bool {
	if len(o) == 0 {
		return false
	}
	s := string(o)
	for s != "" {
		var next string
		i := strings.Index(s, ",")
		if i >= 0 {
			// 逗号前半部分和后半部分
			s, next = s[:i], s[i+1:]
		}
		if s == optionName {
			return true
		}
		s = next
	}
	return false
}
