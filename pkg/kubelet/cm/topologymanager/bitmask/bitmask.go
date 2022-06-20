/*
Copyright 2019 The Kubernetes Authors.

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

package bitmask

import (
	"fmt"
	"math/bits"
	"strconv"
)

// BitMask interface allows hint providers to create BitMasks for TopologyHints
type BitMask interface {
	Add(bits ...int) error
	Remove(bits ...int) error
	And(masks ...BitMask)
	Or(masks ...BitMask)
	Clear()
	Fill()
	IsEqual(mask BitMask) bool
	IsEmpty() bool
	IsSet(bit int) bool
	IsNarrowerThan(mask BitMask) bool
	String() string
	Count() int
	GetBits() []int
}

type bitMask uint64

// NewEmptyBitMask creates a new, empty BitMask
func NewEmptyBitMask() BitMask {
	s := bitMask(0)
	return &s
}

// NewBitMask creates a new BitMask
func NewBitMask(bits ...int) (BitMask, error) {
	// 初始化为0
	s := bitMask(0)
	err := (&s).Add(bits...)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Add adds the bits with topology affinity to the BitMask
// 将bits里每个数值（代表1在这个数字+1位上），组成二进制的mask
func (s *bitMask) Add(bits ...int) error {
	mask := *s
	for _, i := range bits {
		if i < 0 || i >= 64 {
			return fmt.Errorf("bit number must be in range 0-63")
		}
		mask |= 1 << uint64(i)
	}
	*s = mask
	return nil
}

// Remove removes specified bits from BitMask
func (s *bitMask) Remove(bits ...int) error {
	mask := *s
	for _, i := range bits {
		if i < 0 || i >= 64 {
			return fmt.Errorf("bit number must be in range 0-63")
		}
		// and not操作符
		// mask = mask & (^ 1 << uint64(i))
		mask &^= 1 << uint64(i)
	}
	*s = mask
	return nil
}

// And performs and operation on all bits in masks
func (s *bitMask) And(masks ...BitMask) {
	for _, m := range masks {
		*s &= *m.(*bitMask)
	}
}

// Or performs or operation on all bits in masks
func (s *bitMask) Or(masks ...BitMask) {
	for _, m := range masks {
		*s |= *m.(*bitMask)
	}
}

// Clear resets all bits in mask to zero
func (s *bitMask) Clear() {
	*s = 0
}

// Fill sets all bits in mask to one
func (s *bitMask) Fill() {
	*s = bitMask(^uint64(0))
}

// IsEmpty checks mask to see if all bits are zero
func (s *bitMask) IsEmpty() bool {
	return *s == 0
}

// IsSet checks bit in mask to see if bit is set to one
// bit是否在s中
func (s *bitMask) IsSet(bit int) bool {
	if bit < 0 || bit >= 64 {
		return false
	}
	// 1进行左移bit数量位数，然后跟s进行与计算
	// 大于0，代表在s中，否则代表不在s中
	return (*s & (1 << uint64(bit))) > 0
}

// IsEqual checks if masks are equal
func (s *bitMask) IsEqual(mask BitMask) bool {
	return *s == *mask.(*bitMask)
}

// IsNarrowerThan checks if one mask is narrower than another.
//
// A mask is said to be "narrower" than another if it has lets bits set. If the
// same number of bits are set in both masks, then the mask with more
// lower-numbered bits set wins out.
// 如果位的值为1的数量相等，且s小于mask，则返回true
// 位的值为1的数量相等，且s不小于mask，返回false
// s的位的值为1的数量，小于mask里位的值为1的数量，返回true
// s的位的值为1的数量，大于等于mask里位的值为1的数量，返回false
func (s *bitMask) IsNarrowerThan(mask BitMask) bool {
	// 如果位的值为1的数量相等，且s小于mask，则返回true
	if s.Count() == mask.Count() {
		if *s < *mask.(*bitMask) {
			return true
		}
	}
	// 如果位的值为1的数量相等，且s不小于mask
	// 或位的值为1的数量不相等
	// 则比较s的位的值为1的数量，是否小于mask里位的值为1的数量
	// 所以，位的值为1的数量相等，且s不小于mask，返回false
	// s的位的值为1的数量，小于mask里位的值为1的数量，返回true
	// s的位的值为1的数量，大于等于mask里位的值为1的数量，返回false
	return s.Count() < mask.Count()
}

// String converts mask to string
func (s *bitMask) String() string {
	grouping := 2
	for shift := 64 - grouping; shift > 0; shift -= grouping {
		if *s > (1 << uint(shift)) {
			return fmt.Sprintf("%0"+strconv.Itoa(shift+grouping)+"b", *s)
		}
	}
	return fmt.Sprintf("%0"+strconv.Itoa(grouping)+"b", *s)
}

// Count counts number of bits in mask set to one
func (s *bitMask) Count() int {
	return bits.OnesCount64(uint64(*s))
}

// Getbits returns each bit number with bits set to one
// 返回s转为二进制数里为1的index
func (s *bitMask) GetBits() []int {
	var bits []int
	for i := uint64(0); i < 64; i++ {
		// 1左移i位，然后跟s进行与计算，如果大于0（s里这一位是1），则添加i到bits中
		if (*s & (1 << i)) > 0 {
			bits = append(bits, int(i))
		}
	}
	return bits
}

// And is a package level implementation of 'and' between first and masks
func And(first BitMask, masks ...BitMask) BitMask {
	s := *first.(*bitMask)
	s.And(masks...)
	return &s
}

// Or is a package level implementation of 'or' between first and masks
func Or(first BitMask, masks ...BitMask) BitMask {
	s := *first.(*bitMask)
	s.Or(masks...)
	return &s
}

// IterateBitMasks iterates all possible masks from a list of bits,
// issuing a callback on each mask.
// 依次从bits中按顺序取1个item、2个item、3个item，执行callback
func IterateBitMasks(bits []int, callback func(BitMask)) {
	var iterate func(bits, accum []int, size int)
	iterate = func(bits, accum []int, size int) {
		// accum大小等于遍历的次数
		if len(accum) == size {
			// 将bits里每个数值（代表1在这个数字+1位上），组成二进制的mask
			mask, _ := NewBitMask(accum...)
			callback(mask)
			return
		}
		// 0：bits[1:]、bits[0]、1，执行
		// 1：bits[2:]、bits[0] bits[1]、1，不执行

		// 0：bits[1:]、bits[0]、2，不执行
		// 1：bits[2:]、bits[0] bits[1]、2，执行

		// accum大小不等于遍历的次数
		// 执行按顺序取size个item，执行callback
		for i := range bits {
			iterate(bits[i+1:], append(accum, bits[i]), size)
		}
	}

	for i := 1; i <= len(bits); i++ {
		// bits、空、 1
		// bits、空、2
		iterate(bits, []int{}, i)
	}
}
