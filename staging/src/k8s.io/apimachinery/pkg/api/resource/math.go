/*
Copyright 2014 The Kubernetes Authors.

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

package resource

import (
	"math/big"

	inf "gopkg.in/inf.v0"
)

const (
	// maxInt64Factors is the highest value that will be checked when removing factors of 10 from an int64.
	// It is also the maximum decimal digits that can be represented with an int64.
	// 为什么是18，可能是去除了第一个数字由于不能取满值--百分百保证18位精确，最大int64是19位
	maxInt64Factors = 18
)

var (
	// Commonly needed big.Int values-- treat as read only!
	bigTen      = big.NewInt(10)
	bigZero     = big.NewInt(0)
	bigOne      = big.NewInt(1)
	bigThousand = big.NewInt(1000)
	big1024     = big.NewInt(1024)

	// Commonly needed inf.Dec values-- treat as read only!
	decZero = inf.NewDec(0, 0)
	decOne  = inf.NewDec(1, 0)

	// Largest (in magnitude) number allowed.
	maxAllowed = infDecAmount{inf.NewDec((1<<63)-1, 0)} // == max int64

	// The maximum value we can represent milli-units for.
	// Compare with the return value of Quantity.Value() to
	// see if it's safe to use Quantity.MilliValue().
	MaxMilliValue = int64(((1 << 63) - 1) / 1000)
)

const mostNegative = -(mostPositive + 1)
const mostPositive = 1<<63 - 1

// int64Add returns a+b, or false if that would overflow int64.
func int64Add(a, b int64) (int64, bool) {
	c := a + b
	switch {
	case a > 0 && b > 0:
		if c < 0 {
			return 0, false
		}
	case a < 0 && b < 0:
		if c > 0 {
			return 0, false
		}
		if a == mostNegative && b == mostNegative {
			return 0, false
		}
	}
	return c, true
}

// int64Multiply returns a*b, or false if that would overflow or underflow int64.
// 当a等于1或b等于1，有可能超出int64所表示的范围，这里没有严格限制--需要调用方限制
// 当a或b等于最小的int64数，则返回0和false--就不考虑乘以-1变成最大的int64数
func int64Multiply(a, b int64) (int64, bool) {
	if a == 0 || b == 0 || a == 1 || b == 1 {
		return a * b, true
	}
	if a == mostNegative || b == mostNegative {
		return 0, false
	}
	c := a * b
	// 如果超出了int64，c/b不等于a，返回false
	return c, c/b == a
}

// int64MultiplyScale returns a*b, assuming b is greater than one, or false if that would overflow or underflow int64.
// Use when b is known to be greater than one.
// 假设b是大于1--需要调用方限制
// 当a等于1，有可能超出int64所表示的范围，这里没有严格限制--需要调用方限制
// 当a等于最小的int64数返回且b不等于1，返回0和false--感觉代码写的怪怪的很矛盾
func int64MultiplyScale(a int64, b int64) (int64, bool) {
	if a == 0 || a == 1 {
		return a * b, true
	}
	if a == mostNegative && b != 1 {
		return 0, false
	}
	c := a * b
	// 如果超出了int64，c/b不等于a，返回false
	return c, c/b == a
}

// int64MultiplyScale10 multiplies a by 10, or returns false if that would overflow. This method is faster than
// int64Multiply(a, 10) because the compiler can optimize constant factor multiplication.
func int64MultiplyScale10(a int64) (int64, bool) {
	if a == 0 || a == 1 {
		return a * 10, true
	}
	if a == mostNegative {
		return 0, false
	}
	c := a * 10
	return c, c/10 == a
}

// int64MultiplyScale100 multiplies a by 100, or returns false if that would overflow. This method is faster than
// int64Multiply(a, 100) because the compiler can optimize constant factor multiplication.
func int64MultiplyScale100(a int64) (int64, bool) {
	if a == 0 || a == 1 {
		return a * 100, true
	}
	if a == mostNegative {
		return 0, false
	}
	c := a * 100
	return c, c/100 == a
}

// int64MultiplyScale1000 multiplies a by 1000, or returns false if that would overflow. This method is faster than
// int64Multiply(a, 1000) because the compiler can optimize constant factor multiplication.
func int64MultiplyScale1000(a int64) (int64, bool) {
	if a == 0 || a == 1 {
		return a * 1000, true
	}
	if a == mostNegative {
		return 0, false
	}
	c := a * 1000
	return c, c/1000 == a
}

// positiveScaleInt64 multiplies base by 10^scale, returning false if the
// value overflows. Passing a negative scale is undefined.
// 计算base乘以10^scale的值
// 如果值超出了int64表示的数值范围，则会返回0和false
func positiveScaleInt64(base int64, scale Scale) (int64, bool) {
	switch scale {
	case 0:
		return base, true
	case 1:
		return int64MultiplyScale10(base)
	case 2:
		return int64MultiplyScale100(base)
	case 3:
		return int64MultiplyScale1000(base)
	// 为什么要单独写
	case 6:
		return int64MultiplyScale(base, 1000000)
	// 为什么要单独写
	case 9:
		return int64MultiplyScale(base, 1000000000)
	default:
		value := base
		var ok bool
		for i := Scale(0); i < scale; i++ {
			if value, ok = int64MultiplyScale(value, 10); !ok {
				// 超出int64表示范围
				return 0, false
			}
		}
		return value, true
	}
}

// negativeScaleInt64 reduces base by the provided scale, rounding up, until the
// value is zero or the scale is reached. Passing a negative scale is undefined.
// The value returned, if not exact, is rounded away from zero.
// 以 result*10^scale方式表示base，如果base除以10^scale有余数，则会丢失精度--result（base除以10^scale的值）会进行向上取整
// excat为false代表丢失精度--执行了向上取整，excat为true代表精度未丢失
func negativeScaleInt64(base int64, scale Scale) (result int64, exact bool) {
	if scale == 0 {
		return base, true
	}

	value := base
	var fraction bool
	for i := Scale(0); i < scale; i++ {
		if !fraction && value%10 != 0 {
			fraction = true
		}
		value = value / 10
		// base位数小于等于scale，比如base为200，scale为3
		// 如果value为0，说明已经除完了
		// base不能对10进行整除，如果base为正数返回1和false，如果base为负数返回-1和false
		// base能对10进行整除，返回0和true
		if value == 0 {
			if fraction {
				if base > 0 {
					return 1, false
				}
				return -1, false
			}
			return 0, true
		}
	}
	// base位数大于scale，比如base为200，scale为2

	// base位数大于scale且不是10的倍数，比如base为201，scale为2
	// base是正数，返回value加1和false--代表不精确
	// base是负数，返回value减1和false
	if fraction {
		if base > 0 {
			value++
		} else {
			value--
		}
	}
	// base位数大于scale且是10的倍数，则返回value和true，表示精确
	return value, !fraction
}

func pow10Int64(b int64) int64 {
	switch b {
	case 0:
		return 1
	case 1:
		return 10
	case 2:
		return 100
	case 3:
		return 1000
	case 4:
		return 10000
	case 5:
		return 100000
	case 6:
		return 1000000
	case 7:
		return 10000000
	case 8:
		return 100000000
	case 9:
		return 1000000000
	case 10:
		return 10000000000
	case 11:
		return 100000000000
	case 12:
		return 1000000000000
	case 13:
		return 10000000000000
	case 14:
		return 100000000000000
	case 15:
		return 1000000000000000
	case 16:
		return 10000000000000000
	case 17:
		return 100000000000000000
	case 18:
		return 1000000000000000000
	default:
		return 0
	}
}

// negativeScaleInt64 returns the result of dividing base by scale * 10 and the remainder, or
// false if no such division is possible. Dividing by negative scales is undefined.
// 计算除以10^scale的值和余数和是否可除（scale大于等于18不可除）
func divideByScaleInt64(base int64, scale Scale) (result, remainder int64, exact bool) {
	if scale == 0 {
		return base, 0, true
	}
	// the max scale representable in base 10 in an int64 is 18 decimal places
	if scale >= 18 {
		return 0, base, false
	}
	divisor := pow10Int64(int64(scale))
	return base / divisor, base % divisor, true
}

// removeInt64Factors divides in a loop; the return values have the property that
// value == result * base ^ scale
func removeInt64Factors(value int64, base int64) (result int64, times int32) {
	times = 0
	result = value
	negative := result < 0
	// 负数转成正数做下面的计算
	if negative {
		result = -result
	}
	switch base {
	// allow the compiler to optimize the common cases
	case 10:
		// 一直除以10，直到不能跟10整除
		for result >= 10 && result%10 == 0 {
			times++
			result = result / 10
		}
	// allow the compiler to optimize the common cases
	case 1024:
		// 一直除以1024，直到不能跟1024整除
		for result >= 1024 && result%1024 == 0 {
			times++
			result = result / 1024
		}
	default:
		// 一直除以base，直到不能跟base整除
		for result >= base && result%base == 0 {
			times++
			result = result / base
		}
	}
	// 还原回负数
	if negative {
		result = -result
	}
	return result, times
}

// removeBigIntFactors divides in a loop; the return values have the property that
// d == result * factor ^ times
// d may be modified in place.
// If d == 0, then the return values will be (0, 0)
func removeBigIntFactors(d, factor *big.Int) (result *big.Int, times int32) {
	q := big.NewInt(0)
	m := big.NewInt(0)
	for d.Cmp(bigZero) != 0 {
		// q等于d除以factor，m为余数 
		q.DivMod(d, factor, m)
		// 余数不等于0,就终止循环
		if m.Cmp(bigZero) != 0 {
			break
		}
		times++
		// d变成除完之后的值，q等于之前的d（这个感觉没什么用）
		d, q = q, d
	}
	return d, times
}
