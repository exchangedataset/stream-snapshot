package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strconv"
	"time"
	"unsafe"

	"github.com/exchangedataset/streamcommons"
	"github.com/exchangedataset/streamcommons/formatter"
	"github.com/exchangedataset/streamcommons/simulator"
)

// SnapshotParameter is the parameter for snapshot
type SnapshotParameter struct {
	exchange   string
	nanosec    int64
	minute     int64
	channels   []string
	format     string
	postFilter map[string]bool
}

func feedToSimulator(reader *bufio.Reader, targetNanosec int64, sim *simulator.Simulator, setNewSim func(*simulator.Simulator) error) (scanned int, stop bool, err error) {
	tprocess := int64(0)
	for {
		// read type str
		typeBytes, serr := reader.ReadBytes('\t')
		if serr != nil {
			if serr == io.EOF {
				break
			} else {
				// some error
				err = serr
				return
			}
		}
		scanned += len(typeBytes)
		typeStr := *(*string)(unsafe.Pointer(&typeBytes))
		// read timestamp
		var timestampBytes []byte
		if typeStr == "end\t" {
			timestampBytes, err = reader.ReadBytes('\n')
		} else {
			timestampBytes, err = reader.ReadBytes('\t')
		}
		if err != nil {
			return
		}
		scanned += len(timestampBytes)
		if typeStr != "state\t" {
			timestampStr := *(*string)(unsafe.Pointer(&timestampBytes))
			// remove the last character on timestampStr because it is TAB
			var timestamp int64
			timestamp, err = strconv.ParseInt(timestampStr[:len(timestampStr)-1], 10, 64)
			if err != nil {
				return
			}
			if timestamp > targetNanosec {
				// lines after the target time is not needed to construct a snapshot
				// unless it is not a state line
				// state lines should be considered when the target time is before status lines
				// but it have not read first dataset to know the "initial state"
				stop = true
				return
			}
		}
		if typeStr == "msg\t" || typeStr == "state\t" {
			// get channel
			var channelBytes []byte
			channelBytes, err = reader.ReadBytes('\t')
			if err != nil {
				return
			}
			scanned += len(channelBytes)
			channelTrimmedBytes := channelBytes[:len(channelBytes)-1]
			channelTrimmed := *(*string)(unsafe.Pointer(&channelTrimmedBytes))
			// should this channel be passed to simulator?
			var line []byte
			line, err = reader.ReadBytes('\n')
			if err != nil {
				return
			}
			scanned += len(line)
			st := time.Now()
			if typeStr == "msg\t" {
				err = (*sim).ProcessMessageChannelKnown(channelTrimmed, line)
			} else if typeStr == "state\t" {
				err = (*sim).ProcessState(channelTrimmed, line)
			}
			tprocess += time.Now().Sub(st).Nanoseconds()
			if err != nil {
				return
			}
			continue
		} else if typeStr == "start\t" {
			url, serr := reader.ReadBytes('\n')
			if serr != nil {
				return 0, false, serr
			}
			scanned += len(url)
			err = setNewSim(sim)
			if err != nil {
				return
			}
			st := time.Now()
			err = (*sim).ProcessStart(url)
			tprocess += time.Now().Sub(st).Nanoseconds()
			if err != nil {
				return
			}
			continue
		}

		// ignore this line
		var skipped []byte
		skipped, err = reader.ReadBytes('\n')
		scanned += len(skipped)
		if err != nil {
			return
		}
	}
	fmt.Printf("total processing time : %d\n", tprocess)
	return
}

func feed(reader io.ReadCloser, targetNanosec int64, channels []string, sim *simulator.Simulator, setNewSim func(*simulator.Simulator) error) (scanned int, stop bool, err error) {
	defer func() {
		serr := reader.Close()
		if serr != nil {
			if err != nil {
				err = fmt.Errorf("%v, original error was: %v", serr, err)
			} else {
				err = serr
			}
			return
		}
	}()
	var greader *gzip.Reader
	greader, err = gzip.NewReader(reader)
	if err != nil {
		return
	}
	// to ensure closing readers
	defer func() {
		serr := greader.Close()
		if serr != nil {
			if err != nil {
				err = fmt.Errorf("%v, original error was: %v", serr, err)
			} else {
				err = serr
			}
			return
		}
	}()
	breader := bufio.NewReader(greader)
	scanned, stop, err = feedToSimulator(breader, targetNanosec, sim, setNewSim)
	return
}

func snapshot(param SnapshotParameter, bodies *streamcommons.S3GetConcurrent) (ret []byte, totalScanned int64, externalErr error, err error) {
	st := time.Now()
	// check if it has the right simulator for this request
	setNewSim := func(simp *simulator.Simulator) error {
		sim, serr := simulator.GetSimulator(param.exchange, param.channels)
		if serr != nil {
			return serr
		}
		*simp = sim
		return nil
	}
	sim := new(simulator.Simulator)
	serr := setNewSim(sim)
	if serr != nil {
		externalErr = serr
		return
	}
	var form formatter.Formatter
	if param.format != "raw" {
		// check if it has the right formatter for this exhcange and format
		form, serr = formatter.GetFormatter(param.exchange, param.channels, param.format)
		if serr != nil {
			externalErr = serr
			return
		}
	}
	i := 0
	for {
		body, ok := bodies.Next()
		if !ok {
			break
		}
		if body == nil {
			fmt.Printf("skipping file %d: did not exist\n", i)
			continue
		}
		fmt.Printf("reading file %d : %d\n", i, time.Now().Sub(st))
		scanned, stop, serr := feed(body, param.nanosec, param.channels, sim, setNewSim)
		totalScanned += int64(scanned)
		if serr != nil {
			err = serr
			return
		}
		if stop {
			// it is enough to make snapshot
			break
		}
		i++
	}
	buf := make([]byte, 0, 10*1024*1024)
	buffer := bytes.NewBuffer(buf)
	snapshots, serr := (*sim).TakeSnapshot()
	if serr != nil {
		err = serr
		return
	}
	for _, snapshot := range snapshots {
		if form != nil {
			// if formatter is specified, write formatted
			formatted, serr := form.FormatMessage(snapshot.Channel, snapshot.Snapshot)
			if serr != nil {
				err = serr
				return
			}
			for _, f := range formatted {
				if _, ok := param.postFilter[f.Channel]; !ok {
					continue
				}
				nanosecStr := strconv.FormatInt(param.nanosec, 10)
				if _, err = buffer.WriteString(nanosecStr); err != nil {
					return
				}
				if _, err = buffer.WriteRune('\t'); err != nil {
					return
				}
				if _, err = buffer.WriteString(f.Channel); err != nil {
					return
				}
				if _, err = buffer.WriteRune('\t'); err != nil {
					return
				}
				if _, err = buffer.Write(f.Message); err != nil {
					return
				}
				if _, err = buffer.WriteRune('\n'); err != nil {
					return
				}
			}
		} else {
			nanosecStr := strconv.FormatInt(param.nanosec, 10)
			if _, err = buffer.WriteString(nanosecStr); err != nil {
				return
			}
			if _, err = buffer.WriteRune('\t'); err != nil {
				return
			}
			if _, err = buffer.WriteString(snapshot.Channel); err != nil {
				return
			}
			if _, err = buffer.WriteRune('\t'); err != nil {
				return
			}
			if _, err = buffer.Write(snapshot.Snapshot); err != nil {
				return
			}
			if _, err = buffer.WriteRune('\n'); err != nil {
				return
			}
		}
	}
	ret = buffer.Bytes()
	return
}
