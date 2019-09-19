package main

import (
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Shopify/sarama"
)

type consumeCmd struct {
	sync.Mutex

	topic       string
	brokers     []string
	tlsCA       string
	tlsCert     string
	tlsCertKey  string
	offsets     map[int32]interval
	timeout     time.Duration
	verbose     bool
	version     sarama.KafkaVersion
	encodeValue string
	encodeKey   string
	pretty      bool
	group       string

	client        sarama.Client
	consumer      sarama.Consumer
	offsetManager sarama.OffsetManager
	poms          map[int32]sarama.PartitionOffsetManager
}

const (
	maxOffset    int64 = 1<<63 - 1
	offsetResume int64 = -3
)

// position represents an position within the Kafka stream.
type position struct {
	// startIsTime specifies which start field is valid.
	// If it's true, the position is specified as a time range
	// in startTime; otherwise it's specified as
	// an offset in startOffset.
	startIsTime bool

	// startOffset holds the starting offset of the position.
	// It can be one of sarama.OffsetOldest, sarama.OffsetNewest
	// or offsetResume to signify a relative starting position.
	// This field is only significant when startIsTime is false.
	startOffset int64

	// startTime holds the starting time of the position.
	// This field is only significant when startIsTime is true.
	startTime timeRange

	// diffIsTime specifies which diff field is valid.
	// If it's true, the difference is specified as an duration
	// in the diffTime field; otherwise it's specified as
	// an offset in diffOffset.
	diffIsTime bool
	diffOffset int64
	diffTime   time.Duration
}

// timeRange holds a time range, from t0 to just before t2..
// This represents the precision specified in a timestamp
// (for example, when a time is specified as a date,
// the time range will include the whole of that day).
type timeRange struct {
	t0, t1 time.Time
}

func (r timeRange) add(d time.Duration) timeRange {
	return timeRange{
		t0: r.t0.Add(d),
		t1: r.t1.Add(d),
	}
}

type interval struct {
	start position
	end   position
}

func (cmd *consumeCmd) resolveOffset(p position, partition int32) (int64, error) {
	if p.startIsTime || p.diffIsTime {
		return 0, fmt.Errorf("time-based positions not yet supported")
	}
	var startOffset int64
	switch p.startOffset {
	case sarama.OffsetNewest, sarama.OffsetOldest:
		off, err := cmd.client.GetOffset(cmd.topic, partition, p.startOffset)
		if err != nil {
			return 0, err
		}
		if p.startOffset == sarama.OffsetNewest {
			// TODO add comment explaining this.
			off--
		}
		startOffset = off
	case offsetResume:
		if cmd.group == "" {
			return 0, fmt.Errorf("cannot resume without -group argument")
		}
		pom := cmd.getPOM(partition)
		startOffset, _ = pom.NextOffset()
	default:
		startOffset = p.startOffset
	}
	return startOffset + p.diffOffset, nil
}

type consumeArgs struct {
	topic       string
	brokers     string
	tlsCA       string
	tlsCert     string
	tlsCertKey  string
	timeout     time.Duration
	offsets     string
	verbose     bool
	version     string
	encodeValue string
	encodeKey   string
	pretty      bool
	group       string
}

func (cmd *consumeCmd) failStartup(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	failf("use \"kt consume -help\" for more information")
}

func (cmd *consumeCmd) parseArgs(as []string) {
	var (
		err  error
		args = cmd.parseFlags(as)
	)

	envTopic := os.Getenv("KT_TOPIC")
	if args.topic == "" {
		if envTopic == "" {
			cmd.failStartup("Topic name is required.")
			return
		}
		args.topic = envTopic
	}
	cmd.topic = args.topic
	cmd.tlsCA = args.tlsCA
	cmd.tlsCert = args.tlsCert
	cmd.tlsCertKey = args.tlsCertKey
	cmd.timeout = args.timeout
	cmd.verbose = args.verbose
	cmd.pretty = args.pretty
	cmd.version = kafkaVersion(args.version)
	cmd.group = args.group

	if args.encodeValue != "string" && args.encodeValue != "hex" && args.encodeValue != "base64" {
		cmd.failStartup(fmt.Sprintf(`unsupported encodevalue argument %#v, only string, hex and base64 are supported.`, args.encodeValue))
		return
	}
	cmd.encodeValue = args.encodeValue

	if args.encodeKey != "string" && args.encodeKey != "hex" && args.encodeKey != "base64" {
		cmd.failStartup(fmt.Sprintf(`unsupported encodekey argument %#v, only string, hex and base64 are supported.`, args.encodeValue))
		return
	}
	cmd.encodeKey = args.encodeKey

	envBrokers := os.Getenv("KT_BROKERS")
	if args.brokers == "" {
		if envBrokers != "" {
			args.brokers = envBrokers
		} else {
			args.brokers = "localhost:9092"
		}
	}
	cmd.brokers = strings.Split(args.brokers, ",")
	for i, b := range cmd.brokers {
		if !strings.Contains(b, ":") {
			cmd.brokers[i] = b + ":9092"
		}
	}

	cmd.offsets, err = parseOffsets(args.offsets, time.Now())
	if err != nil {
		cmd.failStartup(fmt.Sprintf("%s", err))
	}
}

// parseOffsets parses a set of partition-offset specifiers in the following
// syntax. The grammar uses the BNF-like syntax defined in https://golang.org/ref/spec.
// Timestamps relative to the current day are resolved using now as the current time.
//
//	offsets := [ partitionInterval { "," partitionInterval } ]
//
//	partitionInterval :=
//		partition "=" interval |
//		partition |
//		interval
//
//	partition := "all" | number
//
//	interval := [ position ] [ ":" [ position ] ]
//
//	position :=
//		relativePosition |
//		anchorPosition [ relativePosition ]
//
//	anchorPosition := number | "newest" | "oldest" | "resume" | "[" { /^]/ } "]"
//
//	relativePosition := ( "+" | "-" ) (number | duration )
//
//	number := {"0"| "1"| "2"| "3"| "4"| "5"| "6"| "7"| "8"| "9"}
//
//	duration := { number ("h" | "m" | "s" | "ms" | "ns") }
func parseOffsets(str string, now time.Time) (map[int32]interval, error) {
	result := map[int32]interval{}
	for _, partitionInfo := range strings.Split(str, ",") {
		partitionInfo = strings.TrimSpace(partitionInfo)
		// There's a grammatical ambiguity between a partition
		// number and an interval, because both allow a single
		// decimal number. We work around that by trying an explicit
		// partition first.
		p, err := parsePartition(partitionInfo)
		if err == nil {
			result[p] = interval{
				start: oldestPosition(),
				end:   lastPosition(),
			}
			continue
		}
		intervalStr := partitionInfo
		if i := strings.Index(partitionInfo, "="); i >= 0 {
			// There's an explicitly specified partition.
			p, err = parsePartition(partitionInfo[0:i])
			if err != nil {
				return nil, err
			}
			intervalStr = partitionInfo[i+1:]
		} else {
			// No explicit partition, so implicitly use "all".
			p = -1
		}
		intv, err := parseInterval(intervalStr, now)
		if err != nil {
			return nil, err
		}
		result[p] = intv
	}
	return result, nil
}

func parseInterval(s string, now time.Time) (interval, error) {
	if s == "" {
		// An empty string implies all messages.
		return interval{
			start: oldestPosition(),
			end:   lastPosition(),
		}, nil
	}
	startPos, end, err := parsePosition(s, oldestPosition(), now)
	if err != nil {
		return interval{}, err
	}
	if len(end) == 0 {
		// A single position represents the range from there until the end.
		return interval{
			start: startPos,
			end:   lastPosition(),
		}, nil
	}
	if end[0] != ':' {
		return interval{}, fmt.Errorf("invalid interval %q", s)
	}
	end = end[1:]
	endPos, rest, err := parsePosition(end, lastPosition(), now)
	if err != nil {
		return interval{}, err
	}
	if rest != "" {
		return interval{}, fmt.Errorf("invalid interval %q", s)
	}
	return interval{
		start: startPos,
		end:   endPos,
	}, nil
}

func isDigit(r rune) bool {
	return '0' <= r && r <= '9'
}

func isLower(r rune) bool {
	return 'a' <= r && r <= 'z'
}

// parsePosition parses one half of an interval pair
// and returns that offset and any characters remaining in s.
//
// If s is empty, the given default position will be used.
// Note that a position is always terminated by a colon (the
// interval position divider) or the end of the string.
func parsePosition(s string, defaultPos position, now time.Time) (position, string, error) {
	var anchorStr string
	switch {
	case s == "":
		// It's empty - we'll get the default position.
	case s[0] == '[':
		// It looks like a timestamp.
		i := strings.Index(s, "]")
		if i == -1 {
			return position{}, "", fmt.Errorf("no closing ] found in %q", s)
		}
		anchorStr, s = s[0:i+1], s[i+1:]
	case isDigit(rune(s[0])):
		// It looks like an absolute offset anchor; find first non-digit following it.
		i := strings.IndexFunc(s, func(r rune) bool { return !isDigit(r) })
		if i > 0 {
			anchorStr, s = s[0:i], s[i:]
		} else {
			anchorStr, s = s, ""
		}
	case isLower(rune(s[0])):
		// It looks like one of the special anchor position names, such as "oldest";
		// find first non-letter following it.
		i := strings.IndexFunc(s, func(r rune) bool { return !isLower(r) })
		if i > 0 {
			anchorStr, s = s[0:i], s[i:]
		} else {
			anchorStr, s = s, ""
		}
	case s[0] == '+':
		// No anchor and a positive relative pos: anchor at the start.
		defaultPos = oldestPosition()
	case s[0] == '-':
		// No anchor and a negative relative pos: anchor at the end.
		defaultPos = newestPosition()
	default:
		return position{}, "", fmt.Errorf("invalid position %q", s)
	}
	var relStr, rest string
	// Look for the termination of the relative part.
	if i := strings.Index(s, ":"); i >= 0 {
		relStr, rest = s[0:i], s[i:]
	} else {
		relStr, rest = s, ""
	}
	p, err := parseAnchorPos(anchorStr, defaultPos, now)
	if err != nil {
		return position{}, "", err
	}
	if err := parseRelativePosition(relStr, &p); err != nil {
		return position{}, "", err
	}
	if p.startIsTime == p.diffIsTime {
		// We might be able to combine the offset with the diff.
		if p.diffIsTime {
			p.startTime = p.startTime.add(p.diffTime)
			p.diffTime = 0
			p.diffIsTime = false
		} else if p.startOffset >= 0 {
			p.startOffset += p.diffOffset
			p.diffOffset = 0
		}
	}
	return p, rest, nil
}

func parseAnchorPos(s string, defaultPos position, now time.Time) (position, error) {
	if s == "" {
		return defaultPos, nil
	}
	n, err := strconv.ParseUint(s, 10, 63)
	if err == nil {
		// It's an explicit numeric offset.
		return position{
			startOffset: int64(n),
		}, nil
	}
	if err := err.(*strconv.NumError); err.Err == strconv.ErrRange {
		return position{}, fmt.Errorf("anchor offset %q is too large", s)
	}
	if s[0] == '[' {
		// It's a timestamp.
		// Note: parsePosition has already ensured that the string ends
		// with a ] character.
		// TODO support local timezone timestamps (see issue https://github.com/heetch/hkt/issues/3).
		t, err := parseTime(s[1:len(s)-1], false, now)
		if err != nil {
			return position{}, err
		}
		return position{
			startIsTime: true,
			startTime:   t,
		}, nil
	}
	switch s {
	case "newest":
		return newestPosition(), nil
	case "oldest":
		return oldestPosition(), nil
	case "resume":
		return position{startOffset: offsetResume}, nil
	}
	return position{}, fmt.Errorf("invalid anchor position %q", s)
}

// parseRelativePosition parses a relative position, "-10", "+3", "+1h" or "-3m3s"
// into the relative part of p.
//
// The caller has already ensured that s starts with a sign character.
func parseRelativePosition(s string, p *position) error {
	if s == "" {
		return nil
	}
	diff, err := strconv.ParseInt(s, 10, 64)
	if err == nil {
		p.diffIsTime, p.diffOffset = false, diff
		return nil
	}
	if err := err.(*strconv.NumError); err.Err == strconv.ErrRange {
		return fmt.Errorf("offset %q is too large", s)
	}
	// It looks like a duration.
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid relative position %q", s)
	}
	p.diffIsTime, p.diffTime = true, d
	return nil
}

// parsePartition parses a partition number, or the special
// word "all", meaning all partitions.
func parsePartition(s string) (int32, error) {
	if s == "all" {
		return -1, nil
	}
	p, err := strconv.ParseUint(s, 10, 31)
	if err != nil {
		if err := err.(*strconv.NumError); err.Err == strconv.ErrRange {
			return 0, fmt.Errorf("partition number %q is too large", s)
		}
		return 0, fmt.Errorf("invalid partition number %q", s)
	}
	return int32(p), nil
}

// parseTime parses s in one of a range of possible formats, and returns
// the range of time intervals that it represents.
//
// Any missing information in s will be filled in by using information from now.
// If local is true, times without explicit time zones will be interpreted
// relative to now.Location().
func parseTime(s string, local bool, now time.Time) (timeRange, error) {
	var r timeRange
	var err error
	if r.t0, err = time.Parse(time.RFC3339, s); err == nil {
		r.t1 = r.t0
		// RFC3339 always contains an explicit time zone, so we don't need
		// to convert to local time.
		return r, nil
	} else if r.t0, err = time.Parse("2006-01-02", s); err == nil {
		// A whole day.
		r.t1 = r.t0.AddDate(0, 0, 1)
	} else if r.t0, err = time.Parse("2006-01", s); err == nil {
		// A whole month.
		r.t1 = r.t0.AddDate(0, 1, 0)
	} else if r.t0, err = time.Parse("2006", s); err == nil && r.t0.Year() > 2000 {
		// A whole year.
		r.t1 = r.t0.AddDate(1, 0, 0)
	} else if r.t0, err = time.Parse("15:04", s); err == nil {
		// A minute in the current day. There's an argument that we should choose the closest day
		// that contains the given time (e.g. if the time is 23:30 and the input is 01:20, perhaps
		// we should choose tomorrow morning rather than the morning of the current day).
		r.t0 = time.Date(now.Year(), now.Month(), now.Day(), r.t0.Hour(), r.t0.Minute(), 0, 0, time.UTC)
		r.t1 = r.t0.Add(time.Minute)
	} else if r.t0, err = time.Parse("15:04:05", s); err == nil {
		// An exact moment in the current day.
		r.t0 = time.Date(now.Year(), now.Month(), now.Day(), r.t0.Hour(), r.t0.Minute(), r.t0.Second(), r.t0.Nanosecond(), time.UTC)
		r.t1 = r.t0
	} else if r.t0, err = time.Parse("3pm", s); err == nil {
		// An hour in the current day.
		r.t0 = time.Date(now.Year(), now.Month(), now.Day(), r.t0.Hour(), 0, 0, 0, time.UTC)
		r.t1 = r.t0.Add(time.Hour)
	} else {
		return timeRange{}, fmt.Errorf("invalid timestamp %q", s)
	}
	if local {
		r.t0 = timeWithLocation(r.t0, now.Location())
		r.t1 = timeWithLocation(r.t1, now.Location())
	}
	return r, nil
}

func timeWithLocation(t time.Time, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
}

func oldestPosition() position {
	return position{startOffset: sarama.OffsetOldest}
}

func newestPosition() position {
	return position{startOffset: sarama.OffsetNewest}
}

func lastPosition() position {
	return position{startOffset: maxOffset}
}

func (cmd *consumeCmd) parseFlags(as []string) consumeArgs {
	var args consumeArgs
	flags := flag.NewFlagSet("consume", flag.ContinueOnError)
	flags.StringVar(&args.topic, "topic", "", "Topic to consume (required).")
	flags.StringVar(&args.brokers, "brokers", "", "Comma separated list of brokers. Port defaults to 9092 when omitted (defaults to localhost:9092).")
	flags.StringVar(&args.tlsCA, "tlsca", "", "Path to the TLS certificate authority file")
	flags.StringVar(&args.tlsCert, "tlscert", "", "Path to the TLS client certificate file")
	flags.StringVar(&args.tlsCertKey, "tlscertkey", "", "Path to the TLS client certificate key file")
	flags.StringVar(&args.offsets, "offsets", "", "Specifies what messages to read by partition and offset range (defaults to all).")
	flags.DurationVar(&args.timeout, "timeout", time.Duration(0), "Timeout after not reading messages (default 0 to disable).")
	flags.BoolVar(&args.verbose, "verbose", false, "More verbose logging to stderr.")
	flags.BoolVar(&args.pretty, "pretty", true, "Control output pretty printing.")
	flags.StringVar(&args.version, "version", "", "Kafka protocol version")
	flags.StringVar(&args.encodeValue, "encodevalue", "string", "Present message value as (string|hex|base64), defaults to string.")
	flags.StringVar(&args.encodeKey, "encodekey", "string", "Present message key as (string|hex|base64), defaults to string.")
	flags.StringVar(&args.group, "group", "", "Consumer group to use for marking offsets. kt will mark offsets if this arg is supplied.")

	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage of consume:")
		flags.PrintDefaults()
		fmt.Fprintln(os.Stderr, consumeDocString)
	}

	err := flags.Parse(as)
	if err != nil && strings.Contains(err.Error(), "flag: help requested") {
		os.Exit(0)
	} else if err != nil {
		os.Exit(2)
	}

	return args
}

func (cmd *consumeCmd) setupClient() {
	var (
		err error
		usr *user.User
		cfg = sarama.NewConfig()
	)
	cfg.Version = cmd.version
	if usr, err = user.Current(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read current user err=%v", err)
	}
	cfg.ClientID = "kt-consume-" + sanitizeUsername(usr.Username)
	if cmd.verbose {
		fmt.Fprintf(os.Stderr, "sarama client configuration %#v\n", cfg)
	}
	tlsConfig, err := setupCerts(cmd.tlsCert, cmd.tlsCA, cmd.tlsCertKey)
	if err != nil {
		failf("failed to setup certificates err=%v", err)
	}
	if tlsConfig != nil {
		cfg.Net.TLS.Enable = true
		cfg.Net.TLS.Config = tlsConfig
	}

	if cmd.client, err = sarama.NewClient(cmd.brokers, cfg); err != nil {
		failf("failed to create client err=%v", err)
	}
}

func (cmd *consumeCmd) run(args []string) {
	var err error

	cmd.parseArgs(args)

	if cmd.verbose {
		sarama.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}

	cmd.setupClient()
	cmd.setupOffsetManager()

	if cmd.consumer, err = sarama.NewConsumerFromClient(cmd.client); err != nil {
		failf("failed to create consumer err=%v", err)
	}
	defer logClose("consumer", cmd.consumer)

	partitions := cmd.findPartitions()
	if len(partitions) == 0 {
		failf("Found no partitions to consume")
	}
	defer cmd.closePOMs()

	cmd.consume(partitions)
}

func (cmd *consumeCmd) setupOffsetManager() {
	if cmd.group == "" {
		return
	}

	var err error
	if cmd.offsetManager, err = sarama.NewOffsetManagerFromClient(cmd.group, cmd.client); err != nil {
		failf("failed to create offsetmanager err=%v", err)
	}
}

func (cmd *consumeCmd) consume(partitions []int32) {
	var (
		wg  sync.WaitGroup
		out = make(chan printContext)
	)

	go print(out, cmd.pretty)

	wg.Add(len(partitions))
	for _, p := range partitions {
		go func(p int32) { defer wg.Done(); cmd.consumePartition(out, p) }(p)
	}
	wg.Wait()
}

func (cmd *consumeCmd) consumePartition(out chan printContext, partition int32) {
	var (
		offsets interval
		err     error
		pcon    sarama.PartitionConsumer
		start   int64
		end     int64
		ok      bool
	)

	if offsets, ok = cmd.offsets[partition]; !ok {
		offsets, ok = cmd.offsets[-1]
	}

	if start, err = cmd.resolveOffset(offsets.start, partition); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read start offset for partition %v err=%v\n", partition, err)
		return
	}

	if end, err = cmd.resolveOffset(offsets.end, partition); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read end offset for partition %v err=%v\n", partition, err)
		return
	}

	if pcon, err = cmd.consumer.ConsumePartition(cmd.topic, partition, start); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to consume partition %v err=%v\n", partition, err)
		return
	}

	cmd.partitionLoop(out, pcon, partition, end)
}

type consumedMessage struct {
	Partition int32      `json:"partition"`
	Offset    int64      `json:"offset"`
	Key       *string    `json:"key"`
	Value     *string    `json:"value"`
	Timestamp *time.Time `json:"timestamp,omitempty"`
}

func newConsumedMessage(m *sarama.ConsumerMessage, encodeKey, encodeValue string) consumedMessage {
	result := consumedMessage{
		Partition: m.Partition,
		Offset:    m.Offset,
		Key:       encodeBytes(m.Key, encodeKey),
		Value:     encodeBytes(m.Value, encodeValue),
	}

	if !m.Timestamp.IsZero() {
		result.Timestamp = &m.Timestamp
	}

	return result
}

func encodeBytes(data []byte, encoding string) *string {
	if data == nil {
		return nil
	}

	var str string
	switch encoding {
	case "hex":
		str = hex.EncodeToString(data)
	case "base64":
		str = base64.StdEncoding.EncodeToString(data)
	default:
		str = string(data)
	}

	return &str
}

func (cmd *consumeCmd) closePOMs() {
	cmd.Lock()
	for p, pom := range cmd.poms {
		if err := pom.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close partition offset manager for partition %v err=%v", p, err)
		}
	}
	cmd.Unlock()
}

func (cmd *consumeCmd) getPOM(p int32) sarama.PartitionOffsetManager {
	cmd.Lock()
	if cmd.poms == nil {
		cmd.poms = map[int32]sarama.PartitionOffsetManager{}
	}
	pom, ok := cmd.poms[p]
	if ok {
		cmd.Unlock()
		return pom
	}

	pom, err := cmd.offsetManager.ManagePartition(cmd.topic, p)
	if err != nil {
		cmd.Unlock()
		failf("failed to create partition offset manager err=%v", err)
	}
	cmd.poms[p] = pom
	cmd.Unlock()
	return pom
}

func (cmd *consumeCmd) partitionLoop(out chan printContext, pc sarama.PartitionConsumer, p int32, end int64) {
	defer logClose(fmt.Sprintf("partition consumer %v", p), pc)
	var (
		timer   *time.Timer
		pom     sarama.PartitionOffsetManager
		timeout = make(<-chan time.Time)
	)

	if cmd.group != "" {
		pom = cmd.getPOM(p)
	}

	for {
		if cmd.timeout > 0 {
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(cmd.timeout)
			timeout = timer.C
		}

		select {
		case <-timeout:
			fmt.Fprintf(os.Stderr, "consuming from partition %v timed out after %s\n", p, cmd.timeout)
			return
		case err := <-pc.Errors():
			fmt.Fprintf(os.Stderr, "partition %v consumer encountered err %s", p, err)
			return
		case msg, ok := <-pc.Messages():
			if !ok {
				fmt.Fprintf(os.Stderr, "unexpected closed messages chan")
				return
			}

			m := newConsumedMessage(msg, cmd.encodeKey, cmd.encodeValue)
			ctx := printContext{output: m, done: make(chan struct{})}
			out <- ctx
			<-ctx.done

			if cmd.group != "" {
				pom.MarkOffset(msg.Offset+1, "")
			}

			if end > 0 && msg.Offset >= end {
				return
			}
		}
	}
}

func (cmd *consumeCmd) findPartitions() []int32 {
	var (
		all []int32
		res []int32
		err error
	)
	if all, err = cmd.consumer.Partitions(cmd.topic); err != nil {
		failf("failed to read partitions for topic %v err=%v", cmd.topic, err)
	}

	if _, hasDefault := cmd.offsets[-1]; hasDefault {
		return all
	}

	for _, p := range all {
		if _, ok := cmd.offsets[p]; ok {
			res = append(res, p)
		}
	}

	return res
}

var consumeDocString = `
The values for -topic and -brokers can also be set via environment variables KT_TOPIC and KT_BROKERS respectively.
The values supplied on the command line win over environment variable values.

Offsets can be specified as a comma-separated list of intervals:

  [[partition=start:end],...]

For example:

	3=100:300,5=43:67

would consume from offset 100 to offset 300 inclusive in partition 3,
and from 43 to 67 in partition 5.

The default is to consume from the oldest offset on every partition for the given topic.

 - partition is the numeric identifier for a partition. You can use "all" to
   specify a default interval for all partitions.

 - start is the included offset where consumption should start.

 - end is the included offset where consumption should end.

The following syntax is supported for each offset:

TODO document time-based syntax
	briefly:
		[time-format]
		accepted time formats
		some time formats inherently specify a range
		difference is in time.Duration format
		when there's a time range, we go from earliest of first time to latest of second time

  (oldest|newest|resume)?(+|-)?(\d+)?

 - "oldest" and "newest" refer to the oldest and newest offsets known for a
   given partition.

 - "resume" can only be used in combination with -group.

 - You can use "+" with a numeric value to skip the given number of messages
   since the oldest offset. For example, "1=+20" will skip 20 offset value since
   the oldest offset for partition 1.

 - You can use "-" with a numeric value to refer to only the given number of
   messages before the newest offset. For example, "1=-10" will refer to the
   last 10 offset values before the newest offset for partition 1.

 - Relative offsets are based on numeric values and will not take skipped
   offsets (e.g. due to compaction) into account.

 - Given only a numeric value, it is interpreted as an absolute offset value.

More examples:

To consume messages from partition 0 between offsets 10 and 20 (inclusive).

  0=10:20

To define an interval for all partitions use -1 as the partition identifier:

  all=2:10

You can also override the offsets for a single partition, in this case 2:

  all=1-10,2=5-10

To consume from multiple partitions:

  0=4:,2=1:10,6

This would consume messages from three partitions:

  - Anything from partition 0 starting at offset 4.
  - Messages between offsets 1 and 10 from partition 2.
  - Anything from partition 6.

To start at the latest offset for each partition:

  all=newest:

Or shorter:

  newest:

To consume the last 10 messages:

  newest-10:

To skip the first 15 messages starting with the oldest offset:

  oldest+10:

In both cases you can omit "newest" and "oldest":

  -10:

and

  +10:

Will achieve the same as the two examples above.

`
