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

package logs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/daemon/logger/jsonfilelog/jsonlog"
	"github.com/fsnotify/fsnotify"
	"k8s.io/klog"

	"k8s.io/api/core/v1"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/util/tail"
)

// Notice that the current CRI logs implementation doesn't handle
// log rotation.
// * It will not retrieve logs in rotated log file.
// * If log rotation happens when following the log:
//   * If the rotation is using create mode, we'll still follow the old file.
//   * If the rotation is using copytruncate, we'll be reading at the original position and get nothing.
// TODO(random-liu): Support log rotation.

const (
	// timeFormatOut is the format for writing timestamps to output.
	timeFormatOut = types.RFC3339NanoFixed
	// timeFormatIn is the format for parsing timestamps from other logs.
	timeFormatIn = types.RFC3339NanoLenient

	// logForceCheckPeriod is the period to check for a new read
	logForceCheckPeriod = 1 * time.Second
)

var (
	// eol is the end-of-line sign in the log.
	eol = []byte{'\n'}
	// delimiter is the delimiter for timestamp and stream type in log line.
	delimiter = []byte{' '}
	// tagDelimiter is the delimiter for log tags.
	tagDelimiter = []byte(runtimeapi.LogTagDelimiter)
)

// logMessage is the CRI internal log type.
type logMessage struct {
	timestamp time.Time
	stream    runtimeapi.LogStreamType
	log       []byte
}

// reset resets the log to nil.
func (l *logMessage) reset() {
	l.timestamp = time.Time{}
	l.stream = ""
	l.log = nil
}

// LogOptions is the CRI internal type of all log options.
type LogOptions struct {
	tail      int64
	bytes     int64
	since     time.Time
	follow    bool
	timestamp bool
}

// NewLogOptions convert the v1.PodLogOptions to CRI internal LogOptions.
func NewLogOptions(apiOpts *v1.PodLogOptions, now time.Time) *LogOptions {
	opts := &LogOptions{
		tail:      -1, // -1 by default which means read all logs.
		bytes:     -1, // -1 by default which means read all logs.
		follow:    apiOpts.Follow,
		timestamp: apiOpts.Timestamps,
	}
	if apiOpts.TailLines != nil {
		opts.tail = *apiOpts.TailLines
	}
	if apiOpts.LimitBytes != nil {
		opts.bytes = *apiOpts.LimitBytes
	}
	if apiOpts.SinceSeconds != nil {
		opts.since = now.Add(-time.Duration(*apiOpts.SinceSeconds) * time.Second)
	}
	if apiOpts.SinceTime != nil && apiOpts.SinceTime.After(opts.since) {
		opts.since = apiOpts.SinceTime.Time
	}
	return opts
}

// parseFunc is a function parsing one log line to the internal log type.
// Notice that the caller must make sure logMessage is not nil.
type parseFunc func([]byte, *logMessage) error

var parseFuncs = []parseFunc{
	parseCRILog,        // CRI log format parse function
	parseDockerJSONLog, // Docker JSON log format parse function
}

// parseCRILog parses logs in CRI log format. CRI Log format example:
//   2016-10-06T00:17:09.669794202Z stdout P log content 1
//   2016-10-06T00:17:09.669794203Z stderr F log content 2
func parseCRILog(log []byte, msg *logMessage) error {
	var err error
	// Parse timestamp
	// 找到第一个空格的index
	idx := bytes.Index(log, delimiter)
	if idx < 0 {
		return fmt.Errorf("timestamp is not found")
	}
	// 第一个空格前面是时间
	msg.timestamp, err = time.Parse(timeFormatIn, string(log[:idx]))
	if err != nil {
		return fmt.Errorf("unexpected timestamp format %q: %v", timeFormatIn, err)
	}

	// Parse stream type
	// 去除时间后剩下部分
	log = log[idx+1:]
	// 找到第一个空格的index
	idx = bytes.Index(log, delimiter)
	if idx < 0 {
		return fmt.Errorf("stream type is not found")
	}
	msg.stream = runtimeapi.LogStreamType(log[:idx])
	// 判断streamType是否是stdout或stderr
	if msg.stream != runtimeapi.Stdout && msg.stream != runtimeapi.Stderr {
		return fmt.Errorf("unexpected stream type %q", msg.stream)
	}

	// Parse log tag
	// 去除streamType后剩下部分
	log = log[idx+1:]
	// 找到第一个空格的index
	idx = bytes.Index(log, delimiter)
	if idx < 0 {
		return fmt.Errorf("log tag is not found")
	}
	// Keep this forward compatible.
	// 以":"进行分隔 tag部分
	tags := bytes.Split(log[:idx], tagDelimiter)
	// 如果第一个字符是"P"
	partial := (runtimeapi.LogTag(tags[0]) == runtimeapi.LogTagPartial)
	// Trim the tailing new line if this is a partial line.
	// 如果第一个字符是"P"且log尾部为'\n'，则log为去除尾部'\n'后剩下部分
	if partial && len(log) > 0 && log[len(log)-1] == '\n' {
		log = log[:len(log)-1]
	}

	// Get log content
	// 去除tag后剩下部分
	msg.log = log[idx+1:]

	return nil
}

// parseDockerJSONLog parses logs in Docker JSON log format. Docker JSON log format
// example:
//   {"log":"content 1","stream":"stdout","time":"2016-10-20T18:39:20.57606443Z"}
//   {"log":"content 2","stream":"stderr","time":"2016-10-20T18:39:20.57606444Z"}
func parseDockerJSONLog(log []byte, msg *logMessage) error {
	var l = &jsonlog.JSONLog{}
	l.Reset()

	// TODO: JSON decoding is fairly expensive, we should evaluate this.
	if err := json.Unmarshal(log, l); err != nil {
		return fmt.Errorf("failed with %v to unmarshal log %q", err, l)
	}
	msg.timestamp = l.Created
	msg.stream = runtimeapi.LogStreamType(l.Stream)
	msg.log = []byte(l.Log)
	return nil
}

// getParseFunc returns proper parse function based on the sample log line passed in.
// 先使用cri日志格式来解析，如果报错，则使用docker日志格式进行解析
func getParseFunc(log []byte) (parseFunc, error) {
	for _, p := range parseFuncs {
		if err := p(log, &logMessage{}); err == nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("unsupported log format: %q", log)
}

// logWriter controls the writing into the stream based on the log options.
type logWriter struct {
	stdout io.Writer
	stderr io.Writer
	opts   *LogOptions
	remain int64
}

// errMaximumWrite is returned when all bytes have been written.
var errMaximumWrite = errors.New("maximum write")

// errShortWrite is returned when the message is not fully written.
var errShortWrite = errors.New("short write")

func newLogWriter(stdout io.Writer, stderr io.Writer, opts *LogOptions) *logWriter {
	w := &logWriter{
		stdout: stdout,
		stderr: stderr,
		opts:   opts,
		remain: math.MaxInt64, // initialize it as infinity
	}
	if opts.bytes >= 0 {
		w.remain = opts.bytes
	}
	return w
}

// writeLogs writes logs into stdout, stderr.
func (w *logWriter) write(msg *logMessage) error {
	// 这一行日志时间在w.opts.since前面，直接忽略
	if msg.timestamp.Before(w.opts.since) {
		// Skip the line because it's older than since
		return nil
	}
	line := msg.log
	// 如果需要显示时间戳，转换时间为RFC3339NanoFixed格式，这一行前面添加时间
	if w.opts.timestamp {
		prefix := append([]byte(msg.timestamp.Format(timeFormatOut)), delimiter[0])
		line = append(prefix, line...)
	}
	// If the line is longer than the remaining bytes, cut it.
	// 这一行超出了w.remain，则超出部分会被截断
	if int64(len(line)) > w.remain {
		line = line[:w.remain]
	}
	// Get the proper stream to write to.
	// 判断日志类型（stdout、stderr），写入到不同的地方
	var stream io.Writer
	switch msg.stream {
	case runtimeapi.Stdout:
		stream = w.stdout
	case runtimeapi.Stderr:
		stream = w.stderr
	default:
		return fmt.Errorf("unexpected stream type %q", msg.stream)
	}
	// 写入数据
	n, err := stream.Write(line)
	w.remain -= int64(n)
	if err != nil {
		return err
	}
	// If the line has not been fully written, return errShortWrite
	// 只写入部分数据，返回errShortWrite
	if n < len(line) {
		return errShortWrite
	}
	// 这一行的数据长度小于等于写入的长度n，且写入的长度n大于等于w.remain
	// If there are no more bytes left, return errMaximumWrite
	// 所有数据都被写进去了（还有可能多写了），返回errMaximumWrite
	if w.remain <= 0 {
		return errMaximumWrite
	}
	// 这一行的数据长度小于等于写入的长度n，且写入的长度小于w.remain
	// w.remain还有剩余空间
	return nil
}

// ReadLogs read the container log and redirect into stdout and stderr.
// Note that containerID is only needed when following the log, or else
// just pass in empty string "".
// 当没有设置follow log时候，正常退出条件是: 1.读到文件末尾 或2.达到日志输出的指定的限制大小（手动指定）
// 当设置了follow log时候，正常退出条件是: 当容器不在运行状态
// 如果容器一直在写日志，有可能出现我要最后10行，但是输出了最后11行，因为文件末尾一直发生变化，计算最后n行的偏移量时候的文件结尾和读取数据文件结尾不在一个位置
func ReadLogs(ctx context.Context, path, containerID string, opts *LogOptions, runtimeService internalapi.RuntimeService, stdout, stderr io.Writer) error {
	// fsnotify has different behavior for symlinks in different platform,
	// for example it follows symlink on Linux, but not on Windows,
	// so we explicitly resolve symlinks before reading the logs.
	// There shouldn't be security issue because the container log
	// path is owned by kubelet and the container runtime.
	evaluated, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("failed to try resolving symlinks in path %q: %v", path, err)
	}
	path = evaluated
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open log file %q: %v", path, err)
	}
	defer f.Close()

	// Search start point based on tail line.
	// 返回读取最后n行第一个字节在文件里的偏移量
	start, err := tail.FindTailLineStartIndex(f, opts.tail)
	if err != nil {
		return fmt.Errorf("failed to tail %d lines of log file %q: %v", opts.tail, path, err)
	}
	// 标记下一次读取的位置
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return fmt.Errorf("failed to seek %d in log file %q: %v", start, path, err)
	}

	// Start parsing the logs.
	r := bufio.NewReader(f)
	// Do not create watcher here because it is not needed if `Follow` is false.
	var watcher *fsnotify.Watcher
	var parse parseFunc
	var stop bool
	found := true
	writer := newLogWriter(stdout, stderr, opts)
	msg := &logMessage{}
	for {
		if stop {
			klog.V(2).Infof("Finish parsing log file %q", path)
			return nil
		}
		// 从标记的读取位置，读取第一个换行符"\n"，返回读取的数据
		l, err := r.ReadBytes(eol[0])
		if err != nil {
			if err != io.EOF { // This is an real error
				return fmt.Errorf("failed to read log file %q: %v", path, err)
			}
			// 错误是读到文件的结尾
			// 如果指定了follow日志流
			if opts.follow {
				// The container is not running, we got to the end of the log.
				// 在found, recreated, err = waitLogs(ctx, containerID, watcher, runtimeService)里
				// 只有当容器不在运行状态且调用runtime查询container status没有发生错误，found为false才会执行到这里
				if !found {
					return nil
				}
				// Reset seek so that if this is an incomplete line,
				// it will be read again.
				// 重新标记读取点为上一个读取位置，这里重新读取这一行，以防这一行未结束
				// 只有当这一行完整，才会到解析日志那个步骤。要不然一直在这个opts.follow循环里
				if _, err := f.Seek(-int64(len(l)), io.SeekCurrent); err != nil {
					return fmt.Errorf("failed to reset seek in log file %q: %v", path, err)
				}
				// 添加日志文件的inotify，然后重新开始读取日志文件
				if watcher == nil {
					// Initialize the watcher if it has not been initialized yet.
					if watcher, err = fsnotify.NewWatcher(); err != nil {
						return fmt.Errorf("failed to create fsnotify watcher: %v", err)
					}
					defer watcher.Close()
					if err := watcher.Add(f.Name()); err != nil {
						return fmt.Errorf("failed to watch file %q: %v", f.Name(), err)
					}
					// If we just created the watcher, try again to read as we might have missed
					// the event.
					continue
				}
				var recreated bool
				// 判断日志文件是否发生变化，然后重新读取文件。文件发生变化（创建、重命名、删除、修改权限）则重新打开文件和watch inotify
				// Wait until the next log change.

				// 返回文件是否有新数据和文件是否被重建（创建、重命名、删除、修改权限）和是否发生错误
				// 默认只等待1s，如果文件没有发生任何变化，则认为文件有新数据和文件没有重建和没有错误
				found, recreated, err = waitLogs(ctx, containerID, watcher, runtimeService)
				if err != nil {
					return err
				}
				// 如果日志文件被重建（创建、重命名、删除、修改权限），重新打开文件
				if recreated {
					newF, err := os.Open(path)
					if err != nil {
						// 文件不存在，说明容器被销毁了，则继续读取文件（为什么？），可能是让r.ReadBytes(eol[0])来处理错误（err跟os.Open这个错误不一样？）
						if os.IsNotExist(err) {
							continue
						}
						return fmt.Errorf("failed to open log file %q: %v", path, err)
					}
					// 关闭原来旧文件
					f.Close()
					// 移除原来文件的inotify watch
					if err := watcher.Remove(f.Name()); err != nil && !os.IsNotExist(err) {
						klog.Errorf("failed to remove file watch %q: %v", f.Name(), err)
					}
					f = newF
					// 重新添加日志文件的inotify
					if err := watcher.Add(f.Name()); err != nil {
						return fmt.Errorf("failed to watch file %q: %v", f.Name(), err)
					}
					// 重新建立bufio reader
					r = bufio.NewReader(f)
				}
				// If the container exited consume data until the next EOF
				continue
			}
			// Should stop after writing the remaining content.
			// 没有设置follow log且读到文件末尾，则设置stop为true
			stop = true
			// 没有读到任何数据，直接continue，在循环开头会判断stop为true，直接return nil
			if len(l) == 0 {
				continue
			}
			// 有读到数据，则记录下（没有读到一行就到了文件结尾），继续进行解析日志
			klog.Warningf("Incomplete line in log file %q: %q", path, l)
		}
		// 解析这一行数据
		if parse == nil {
			// Initialize the log parsing function.
			// 设置解析器，为第一个解析日志l成功function，
			// 先使用cri日志格式来解析，如果报错，则使用docker日志格式进行解析
			parse, err = getParseFunc(l)
			if err != nil {
				return fmt.Errorf("failed to get parse function: %v", err)
			}
		}
		// Parse the log line.
		// 重置msg为零值
		msg.reset()
		// 这里进行解析日志l，在parse == nil时候会进行重复解析（这里可以进行优化）
		if err := parse(l, msg); err != nil {
			// 解析出错，则继续循环（读取日志下一行，有可能到文件尾部，判断stop为true直接返回或设置stop为true）
			klog.Errorf("Failed with err %v when parsing log for log file %q: %q", err, path, l)
			continue
		}
		// Write the log line into the stream.
		// 写入到相应目标，根据msg.opts选项，进行过滤或加工。（比如过滤掉不符合指定时间内的日志，长度大于指定的长度）
		if err := writer.write(msg); err != nil {
			// 已经达到需要输出的日志量大小，直接返回nil
			if err == errMaximumWrite {
				klog.V(2).Infof("Finish parsing log file %q, hit bytes limit %d(bytes)", path, opts.bytes)
				return nil
			}
			// 发生错误直接返回
			klog.Errorf("Failed with err %v when writing log for log file %q: %+v", err, path, msg)
			return err
		}
	}
}

// 从runtime中获取容器的运行状态
func isContainerRunning(id string, r internalapi.RuntimeService) (bool, error) {
	s, err := r.ContainerStatus(id)
	if err != nil {
		return false, err
	}
	// Only keep following container log when it is running.
	if s.State != runtimeapi.ContainerState_CONTAINER_RUNNING {
		klog.V(5).Infof("Container %q is not running (state=%q)", id, s.State)
		// Do not return error because it's normal that the container stops
		// during waiting.
		return false, nil
	}
	return true, nil
}

// waitLogs wait for the next log write. It returns two booleans and an error. The first boolean
// indicates whether a new log is found; the second boolean if the log file was recreated;
// the error is error happens during waiting new logs.
// 返回文件是否有新数据和文件是否被重建（创建、重命名、删除、修改权限）和是否发生错误
// 默认只等待1s，如果文件没有发生任何变化，则认为文件有新数据且文件没有重建且没有错误
func waitLogs(ctx context.Context, id string, w *fsnotify.Watcher, runtimeService internalapi.RuntimeService) (bool, bool, error) {
	// no need to wait if the pod is not running
	// 判断container是否在运行状态
	if running, err := isContainerRunning(id, runtimeService); !running {
		return false, false, err
	}
	errRetry := 5
	for {
		select {
		case <-ctx.Done():
			return false, false, fmt.Errorf("context cancelled")
		case e := <-w.Events:
			switch e.Op {
			case fsnotify.Write:
				return true, false, nil
			case fsnotify.Create:
				fallthrough
			case fsnotify.Rename:
				fallthrough
			case fsnotify.Remove:
				fallthrough
			case fsnotify.Chmod:
				return true, true, nil
			default:
				klog.Errorf("Unexpected fsnotify event: %v, retrying...", e)
			}
		case err := <-w.Errors:
			klog.Errorf("Fsnotify watch error: %v, %d error retries remaining", err, errRetry)
			if errRetry == 0 {
				return false, false, err
			}
			errRetry--
		case <-time.After(logForceCheckPeriod):
			return true, false, nil
		}
	}
}
