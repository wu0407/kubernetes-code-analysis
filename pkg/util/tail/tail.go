/*
Copyright 2017 The Kubernetes Authors.

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

package tail

import (
	"bytes"
	"io"
	"io/ioutil"
	"os"
)

const (
	// blockSize is the block size used in tail.
	blockSize = 1024
)

var (
	// eol is the end-of-line sign in the log.
	eol = []byte{'\n'}
)

// ReadAtMost reads at most max bytes from the end of the file identified by path or
// returns an error. It returns true if the file was longer than max. It will
// allocate up to max bytes.
func ReadAtMost(path string, max int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, false, nil
	}
	if size < max {
		max = size
	}
	offset, err := f.Seek(-max, io.SeekEnd)
	if err != nil {
		return nil, false, err
	}
	data, err := ioutil.ReadAll(f)
	return data, offset > 0, err
}

// FindTailLineStartIndex returns the start of last nth line.
// * If n < 0, return the beginning of the file.
// * If n >= 0, return the beginning of last nth line.
// Notice that if the last line is incomplete (no end-of-line), it will not be counted
// as one line.
// 返回读取最后n行第一个字节在文件里的偏移量
// 从尾部开始读取，循环读取1024字节（当到达开头剩下不够1024字节，则读取剩下的字节数），解析1024字节里面有多少行，行数多了就从左边的行开始依次减少行数，如果行数少了，则继续读取1024字节。
// 如果1024字节里面有不是完整一行（没有换行符）的数据，则不会统计成一行，所以输出的偏移量有可能会多计算（在文件中的偏移量会更小或更大）
// 比如1024字节刚好满足需要的行数，1024字节的开头是完整一行，但是尾部并没有换行符，这时候的偏移量就会偏小
// 比如1024字节刚好满足需要的行数，1024字节的开头是不是完整一行，但是尾部是完整一行，这时候偏移量就会偏大
// 比如1024字节刚好满足需要的行数，1024字节的开头是不是完整一行，但是尾部不是完整一行，这时候偏移量准确度未知
func FindTailLineStartIndex(f io.ReadSeeker, n int64) (int64, error) {
	// 如果n小于0，说明从开头第一行开始
	if n < 0 {
		return 0, nil
	}
	// 从相对尾部开始计算位置，0代表从尾部第一行，-1代表从尾部第二行。Seek返回从头开始的行数（尾部是第几个字节，总的文件大小）
	// 这里使用seek标记开始查找点，因为文件可能一直在增长
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	var left, cnt int64
	buf := make([]byte, blockSize)
	// size是文件的大小， right > 0代表还没有到文件开头，right -= blockSize说明每次读取1024字节
	// 当到达开头或已经读取足够的行数了，退出循环
	for right := size; right > 0 && cnt <= n; right -= blockSize {
		left = right - blockSize
		// 剩下的不够1024字节，创建刚好大小的buffer，重置为开头
		if left < 0 {
			left = 0
			buf = make([]byte, right)
		}
		// 设置下次读取的目标偏移量（距离文件开始位置为left，这次seek偏移量比上次seek偏移量小--文件末尾，说明从尾部向头部读取一块数据），发生错误返回错误
		if _, err := f.Seek(left, io.SeekStart); err != nil {
			return 0, err
		}
		// 读取数据放到buf
		if _, err := f.Read(buf); err != nil {
			return 0, err
		}
		// 计算已经读取的行数（没有换行符的行，不统计成一行）
		cnt += int64(bytes.Count(buf, eol))
	}
	//已经读取完成，因为上面在cnt等于n时候，会继续读取，这时cnt有可能大于n，需要减少行数
	for ; cnt > n; cnt-- {
		// 最后一次读取的buf里读取了多行。则计算第一个eof的index，每遇到一个eof，进行cnt减一行

		// 计算第一个eof的index
		idx := bytes.Index(buf, eol) + 1
		// 读取第一个eof之后的数据（剔除第一个eof之前的一行数据）
		buf = buf[idx:]
		// left位置需要向右移动，left要加上idx（由于从尾部开始读取，所以要剔除最左边的行）
		left += int64(idx)
	}
	return left, nil
}
