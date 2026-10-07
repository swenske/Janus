package sysctl

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readTable reads /proc/net/netstat or snmp: pairs of lines, a header of
// names then their values, both after the same prefix ("TcpExt:").
func readTable(path string) (map[string]map[string]int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]int64{}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := 0; i+1 < len(lines); i += 2 {
		names, values := strings.Fields(lines[i]), strings.Fields(lines[i+1])
		if len(names) == 0 || len(names) != len(values) || names[0] != values[0] {
			return nil, fmt.Errorf("%s: line %d doesn't match the line before", path, i+2)
		}
		row := map[string]int64{}
		for j := 1; j < len(names); j++ {
			v, err := strconv.ParseInt(values[j], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", path, names[j], err)
			}
			row[names[j]] = v
		}
		out[strings.TrimSuffix(names[0], ":")] = row
	}
	return out, nil
}

// softnetDropped sums /proc/net/softnet_stat's second column - packets
// dropped because a CPU's input queue was full -, over every CPU.
func softnetDropped(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var total int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		cols := strings.Fields(sc.Text())
		if len(cols) < 2 {
			continue
		}
		v, err := strconv.ParseUint(cols[1], 16, 32)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
		total += int64(v)
	}
	return total, sc.Err()
}

// statColumns sums each column of a /proc/net/stat file - a header of
// names, then a row of hexadecimal values per CPU.
func statColumns(path string) (map[string]int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	names := strings.Fields(lines[0])
	out := map[string]int64{}
	for _, line := range lines[1:] {
		values := strings.Fields(line)
		if len(values) != len(names) {
			return nil, fmt.Errorf("%s: %d values for %d columns", path, len(values), len(names))
		}
		for i, v := range values {
			n, err := strconv.ParseUint(v, 16, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", path, names[i], err)
			}
			out[names[i]] += int64(n)
		}
	}
	return out, nil
}

// readInts reads a /proc/sys file of whitespace-separated integers.
func readInts(path string) ([]int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, f := range strings.Fields(string(data)) {
		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func readInt64(path string) (int64, error) {
	v, err := readInts(path)
	if err != nil {
		return 0, err
	}
	if len(v) != 1 {
		return 0, fmt.Errorf("%s: %d values, want one", path, len(v))
	}
	return v[0], nil
}
