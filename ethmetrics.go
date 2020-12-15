package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type measurement struct {
	name  string
	tags  map[string]string
	value int64
}

// convert snake_case to camelCase
func toCamelCase(s string) string {
	parts := strings.Split(s, "_")
	res := parts[0]
	for _, p := range parts[1:] {
		res += strings.Title(p)
	}
	return res
}

// convert queue_0_tx_unmask_interrupt
// into name=eth.queue.unmaskInterrupt dir=tx queue=0
func getQueueMetric(ms *measurement, name string) bool {
	queueRe := regexp.MustCompile(`queue_(\d+)_(tx|rx)_(.*)`)
	match := queueRe.FindStringSubmatch(name)
	if len(match) != 4 {
		return false
	}
	ms.name = "eth.queue." + toCamelCase(match[3])
	ms.tags = map[string]string{"queue": match[1], "dir": match[2]}
	return true
}

func getRxTxMetric(ms *measurement, name string) bool {
	re := regexp.MustCompile("(rx|tx)_(.*)")
	match := re.FindStringSubmatch(name)
	if len(match) != 3 {
		return false
	}
	ms.name = "eth." + toCamelCase(match[2])
	ms.tags = map[string]string{"dir": match[1]}
	return true
}

func getMeasurement(line string) (measurement, bool) {
	ms := measurement{}
	kv := strings.Split(line, ":")
	if len(kv) != 2 {
		return ms, false
	}
	name := strings.TrimSpace(kv[0])
	valStr := strings.TrimSpace(kv[1])
	if len(name) == 0 || len(valStr) == 0 {
		return ms, false
	}

	val, err := strconv.ParseInt(valStr, 10, 64)
	if err != nil {
		return ms, false
	}
	ms.value = val
	done := false
	if strings.HasPrefix(name, "queue_") {
		done = getQueueMetric(&ms, name)
	}
	if !done && (strings.HasPrefix(name, "tx_") || strings.HasPrefix(name, "rx_")) {
		done = getRxTxMetric(&ms, name)
	}
	if !done {
		ms.name = "eth." + toCamelCase(name)
	}
	return ms, true
}

func getStats(dev string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ethtool", "-S", "eth0")
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("Timed out getting statistics for interface %s\n", dev)
	}
	if err != nil {
		return "", err
	}

	return string(out), nil
}

func toMeasurements(ethtool string) []measurement {
	var res []measurement
	scanner := bufio.NewScanner(strings.NewReader(ethtool))
	for scanner.Scan() {
		m, ok := getMeasurement(scanner.Text())
		if ok {
			res = append(res, m)
		}
	}
	return res
}

func toSpectatord(ms measurement) []byte {
	var b bytes.Buffer
	b.Grow(32)
	b.WriteString("C:")
	b.WriteString(ms.name)
	for k, v := range ms.tags {
		b.WriteByte(',')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
	}
	b.WriteByte(':')
	b.WriteString(strconv.FormatInt(ms.value, 10))
	b.WriteByte('\n')
	return b.Bytes()
}

func measurementsToSpectatord(ms []measurement) [][]byte {
	res := make([][]byte, len(ms))
	for i, m := range ms {
		res[i] = toSpectatord(m)
	}
	return res
}

func getInterfaces() ([]string, error) {
	var res []string
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 && !strings.HasPrefix(iface.Name, "docker") {
			res = append(res, iface.Name)
		}
	}
	return res, nil
}

const SpectatordAddress = "127.0.0.1:1234"

type SpectatordSender struct {
	address string
	c net.Conn
}

func (s *SpectatordSender) initConn() (err error) {
	if s.c != nil {
		_ = s.c.Close()
	}
	s.c, err = net.Dial("udp", s.address)
	return err
}

func NewSpectatordSender(address string) (*SpectatordSender, error) {
	s := SpectatordSender{address, nil}
	err := s.initConn()
	return &s, err
}

func (s *SpectatordSender) sendBatch(batch [][]byte) (err error) {
	chunk := bytes.Join(batch, nil)
	for retry := 1; retry <= 3; retry++ {
		_, err = s.c.Write(chunk)
		if err == nil {
			return
		}
		err = s.initConn() // close and reopen the connection before retrying
		if err != nil {
			return
		}
	}
	return
}

func min(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *SpectatordSender) SendUpdates(updates [][]byte) error {
	beg := 0
	end := len(updates)
	for beg < end {
		cur := min(beg + 32, end)
		err := s.sendBatch(updates[beg : cur])
		if err != nil {
			return err
		}
		beg = cur
	}
	return nil
}

func getDefaultInterfaces() string {
	ifaces, err := getInterfaces()
	if err != nil {
		log.Fatal("Unable to get interfaces", err)
	}
	return strings.Join(ifaces, ",")
}

func main() {
	ifacesStr := getDefaultInterfaces()
	ifacesFlag := flag.String("ifaces", ifacesStr, "Comma separated list of interfaces to query")
	addresssFlag := flag.String("address", SpectatordAddress, "hostname:port where spectatord is listening")
	freqFlag := flag.Duration("frequency", 30 * time.Second, "Collect metrics at this frequency")
	flag.Parse()

	s, err := NewSpectatordSender(*addresssFlag)
	if err != nil {
		log.Fatal("Unable to send metrics to spectatord", err)
	}

	ifaces := strings.Split(*ifacesFlag, ",")
	for {
		start := time.Now()
		for _, dev := range ifaces {
			log.Printf("Gathering ethtool metrics for %s", dev)
			ethtool, err := getStats(dev)
			if err != nil {
				log.Fatal(err)
			}
			ms := toMeasurements(ethtool)
			updates := measurementsToSpectatord(ms)
			err = s.SendUpdates(updates)
			if err != nil {
				log.Printf("Unable to send batch of %d updates: %v", len(updates), err)
			}
		}
		elapsed := time.Since(start)
		toSleep := *freqFlag - elapsed
		devStr := "interface"
		if len(ifaces) > 1 {
			devStr = "interfaces"
		}
		log.Printf("Done processing metrics for %d %s in %v. Sleeping %v", len(ifaces),
			devStr, elapsed, toSleep)
		time.Sleep(toSleep)
	}
}
