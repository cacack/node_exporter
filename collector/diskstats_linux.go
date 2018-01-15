// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build !nodiskstats

package collector

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	diskSubsystem         = "disk"
	diskSectorSize uint64 = 512
)

var (
	ignoredDevices = kingpin.Flag("collector.diskstats.ignored-devices", "Regexp of devices to ignore for diskstats.").Default("^(ram|loop|fd|(h|s|v|xv)d[a-z]|nvme\\d+n\\d+p)\\d+$").String()
)

type diskstatsCollector struct {
	ignoredDevicesPattern *regexp.Regexp
	descs                 []typedDesc
}

func init() {
	registerCollector("diskstats", defaultEnabled, NewDiskstatsCollector)
}

// NewDiskstatsCollector returns a new Collector exposing disk device stats.
func NewDiskstatsCollector() (Collector, error) {
	var diskLabelNames = []string{"device"}

	return &diskstatsCollector{
		ignoredDevicesPattern: regexp.MustCompile(*ignoredDevices),
		// Docs from https://www.kernel.org/doc/Documentation/iostats.txt
		descs: []typedDesc{
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "reads_completed_total"),
					"The total number of reads completed successfully.",
					diskLabelNames,
					nil,
				), valueType: prometheus.CounterValue,
			},
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "reads_merged_total"),
					"The total number of reads merged. See https://www.kernel.org/doc/Documentation/iostats.txt.",
					diskLabelNames,
					nil,
				), valueType: prometheus.CounterValue,
			},
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "read_sectors_total"),
					"The total number of sectors read successfully.",
					diskLabelNames,
					nil,
				), valueType: prometheus.CounterValue,
			},
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "read_time_seconds_total"),
					"The total number of milliseconds spent by all reads.",
					diskLabelNames,
					nil,
				), valueType: prometheus.CounterValue,
			},
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "writes_completed_total"),
					"The total number of writes completed successfully.",
					diskLabelNames,
					nil,
				), valueType: prometheus.CounterValue,
			},
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "writes_merged_total"),
					"The number of writes merged. See https://www.kernel.org/doc/Documentation/iostats.txt.",
					diskLabelNames,
					nil,
				), valueType: prometheus.CounterValue,
			},
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "written_sectors_total"),
					"The total number of sectors written successfully.",
					diskLabelNames,
					nil,
				), valueType: prometheus.CounterValue,
			},
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "write_time_seconds_total"),
					"This is the total number of seconds spent by all writes.",
					diskLabelNames,
					nil,
				), valueType: prometheus.CounterValue,
			},
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "io_now"),
					"The number of I/Os currently in progress.",
					diskLabelNames,
					nil,
				), valueType: prometheus.GaugeValue,
			},
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "io_time_seconds_total"),
					"Total seconds spent doing I/Os.",
					diskLabelNames,
					nil,
				), valueType: prometheus.CounterValue,
			},
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "io_time_weighted_seconds_total"),
					"The weighted # of seconds spent doing I/Os. See https://www.kernel.org/doc/Documentation/iostats.txt.",
					diskLabelNames,
					nil,
				), valueType: prometheus.CounterValue,
			},
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "read_bytes_total"),
					"The total number of bytes read successfully.",
					diskLabelNames,
					nil,
				), valueType: prometheus.CounterValue,
			},
			{
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "written_bytes_total"),
					"The total number of bytes written successfully.",
					diskLabelNames,
					nil,
				), valueType: prometheus.CounterValue,
			},
		},
	}, nil
}

func (c *diskstatsCollector) Update(ch chan<- prometheus.Metric) error {
	procDiskStats := procFilePath("diskstats")
	diskStats, err := getDiskStats()
	if err != nil {
		return fmt.Errorf("couldn't get diskstats: %s", err)
	}

	for dev, stats := range diskStats {
		if c.ignoredDevicesPattern.MatchString(dev) {
			log.Debugf("Ignoring device: %s", dev)
			continue
		}

		if len(stats) != len(c.descs) {
			return fmt.Errorf("invalid line for %s for %s", procDiskStats, dev)
		}

		for i, value := range stats {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("invalid value %s in diskstats: %s", value, err)
			}
			// Convert to seconds
			switch i {
			case 3, 7, 9, 10:
				v = v / 1000.0
			}
			ch <- c.descs[i].mustNewConstMetric(v, dev)
		}
	}
	return nil
}

func getDiskStats() (map[string]map[int]string, error) {
	file, err := os.Open(procFilePath("diskstats"))
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return parseDiskStats(file)
}

func convertDiskSectorsToBytes(sectorCount string) (string, error) {
	sectors, err := strconv.ParseUint(sectorCount, 10, 64)
	if err != nil {
		return "", err
	}

	return strconv.FormatUint(sectors*diskSectorSize, 10), nil
}

func parseDiskStats(r io.Reader) (map[string]map[int]string, error) {
	var (
		diskStats = map[string]map[int]string{}
		scanner   = bufio.NewScanner(r)
	)

	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) < 4 { // we strip major, minor and dev
			return nil, fmt.Errorf("invalid line in %s: %s", procFilePath("diskstats"), scanner.Text())
		}
		dev := parts[2]
		diskStats[dev] = map[int]string{}
		for i, v := range parts[3:] {
			diskStats[dev][i] = v
		}
		bytesRead, err := convertDiskSectorsToBytes(diskStats[dev][2])
		if err != nil {
			return nil, fmt.Errorf("invalid value for sectors read in %s: %s", procFilePath("diskstats"), scanner.Text())
		}
		diskStats[dev][11] = bytesRead

		bytesWritten, err := convertDiskSectorsToBytes(diskStats[dev][6])
		if err != nil {
			return nil, fmt.Errorf("invalid value for sectors written in %s: %s", procFilePath("diskstats"), scanner.Text())
		}
		diskStats[dev][12] = bytesWritten
	}

	return diskStats, scanner.Err()
}
