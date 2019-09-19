package main

import (
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Shopify/sarama"
	qt "github.com/frankban/quicktest"
	"github.com/google/go-cmp/cmp"
)

func TestParseOffsets(t *testing.T) {
	data := []struct {
		testName    string
		input       string
		expected    map[int32]interval
		expectedErr string
	}{
		{
			testName: "empty",
			input:    "",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: sarama.OffsetOldest},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "single-comma",
			input:    ",",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: sarama.OffsetOldest},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "all",
			input:    "all",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: sarama.OffsetOldest},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "oldest",
			input:    "oldest",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: sarama.OffsetOldest},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "resume",
			input:    "resume",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: offsetResume},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "all-with-space",
			input: "	all ",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: sarama.OffsetOldest},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "all-with-zero-initial-offset",
			input:    "all=+0:",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: sarama.OffsetOldest},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "several-partitions",
			input:    "1,2,4",
			expected: map[int32]interval{
				1: interval{
					start: position{startOffset: sarama.OffsetOldest},
					end:   position{startOffset: maxOffset},
				},
				2: interval{
					start: position{startOffset: sarama.OffsetOldest},
					end:   position{startOffset: maxOffset},
				},
				4: interval{
					start: position{startOffset: sarama.OffsetOldest},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "one-partition,empty-offsets",
			input:    "0=",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: sarama.OffsetOldest},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "one-partition,one-offset",
			input:    "0=1",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: 1},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "one-partition,empty-after-colon",
			input:    "0=1:",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: 1},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "multiple-partitions",
			input:    "0=4:,2=1:10,6",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: 4},
					end:   position{startOffset: maxOffset},
				},
				2: interval{
					start: position{startOffset: 1},
					end:   position{startOffset: 10},
				},
				6: interval{
					start: position{startOffset: sarama.OffsetOldest},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "newest-relative",
			input:    "0=-1",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: sarama.OffsetNewest, diffOffset: -1},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "newest-relative,empty-after-colon",
			input:    "0=-1:",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: sarama.OffsetNewest, diffOffset: -1},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "resume-relative",
			input:    "0=resume-10",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: offsetResume, diffOffset: -10},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "oldest-relative",
			input:    "0=+1",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: sarama.OffsetOldest, diffOffset: 1},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "oldest-relative,empty-after-colon",
			input:    "0=+1:",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: sarama.OffsetOldest, diffOffset: 1},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "oldest-relative-to-newest-relative",
			input:    "0=+1:-1",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: sarama.OffsetOldest, diffOffset: 1},
					end:   position{startOffset: sarama.OffsetNewest, diffOffset: -1},
				},
			},
		},
		{
			testName: "specific-partition-with-all-partitions",
			input:    "0=+1:-1,all=1:10",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: sarama.OffsetOldest, diffOffset: 1},
					end:   position{startOffset: sarama.OffsetNewest, diffOffset: -1},
				},
				-1: interval{
					start: position{startOffset: 1, diffOffset: 0},
					end:   position{startOffset: 10, diffOffset: 0},
				},
			},
		},
		{
			testName: "oldest-to-newest",
			input:    "0=oldest:newest",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: sarama.OffsetOldest, diffOffset: 0},
					end:   position{startOffset: sarama.OffsetNewest, diffOffset: 0},
				},
			},
		},
		{
			testName: "oldest-to-newest-with-offsets",
			input:    "0=oldest+10:newest-10",
			expected: map[int32]interval{
				0: interval{
					start: position{startOffset: sarama.OffsetOldest, diffOffset: 10},
					end:   position{startOffset: sarama.OffsetNewest, diffOffset: -10},
				},
			},
		},
		{
			testName: "newest",
			input:    "newest",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: sarama.OffsetNewest, diffOffset: 0},
					end:   position{startOffset: maxOffset, diffOffset: 0},
				},
			},
		},
		{
			testName: "single-partition",
			input:    "10",
			expected: map[int32]interval{
				10: interval{
					start: position{startOffset: sarama.OffsetOldest, diffOffset: 0},
					end:   position{startOffset: maxOffset, diffOffset: 0},
				},
			},
		},
		{
			testName: "single-range,all-partitions",
			input:    "10:20",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: 10},
					end:   position{startOffset: 20},
				},
			},
		},
		{
			testName: "single-range,all-partitions,open-end",
			input:    "10:",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: 10},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName: "all-newest",
			input:    "all=newest:",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: sarama.OffsetNewest, diffOffset: 0},
					end:   position{startOffset: maxOffset, diffOffset: 0},
				},
			},
		},
		{
			testName: "implicit-all-newest-with-offset",
			input:    "newest-10:",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: sarama.OffsetNewest, diffOffset: -10},
					end:   position{startOffset: maxOffset, diffOffset: 0},
				},
			},
		},
		{
			testName: "implicit-all-oldest-with-offset",
			input:    "oldest+10:",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: sarama.OffsetOldest, diffOffset: 10},
					end:   position{startOffset: maxOffset, diffOffset: 0},
				},
			},
		},
		{
			testName: "implicit-all-neg-offset-empty-colon",
			input:    "-10:",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: sarama.OffsetNewest, diffOffset: -10},
					end:   position{startOffset: maxOffset, diffOffset: 0},
				},
			},
		},
		{
			testName: "implicit-all-pos-offset-empty-colon",
			input:    "+10:",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: sarama.OffsetOldest, diffOffset: 10},
					end:   position{startOffset: maxOffset, diffOffset: 0},
				},
			},
		},
		{
			testName: "start-offset-combines-with-diff-offset",
			input:    "1000+3",
			expected: map[int32]interval{
				-1: interval{
					start: position{startOffset: 1003},
					end:   position{startOffset: maxOffset},
				},
			},
		},
		{
			testName:    "invalid-partition",
			input:       "bogus",
			expectedErr: `invalid anchor position "bogus"`,
		},
		{
			testName:    "several-colons",
			input:       ":::",
			expectedErr: `invalid position ":::"`,
		},
		{
			testName:    "bad-relative-offset-start",
			input:       "foo+20",
			expectedErr: `invalid anchor position "foo"`,
		},
		{
			testName:    "bad-relative-offset-diff",
			input:       "oldest+bad",
			expectedErr: `invalid relative position "\+bad"`,
		},
		{
			testName:    "bad-relative-offset-diff-at-start",
			input:       "+bad",
			expectedErr: `invalid relative position "\+bad"`,
		},
		{
			testName:    "relative-offset-too-big",
			input:       "+9223372036854775808",
			expectedErr: `offset "\+9223372036854775808" is too large`,
		},
		{
			testName:    "starting-offset-too-big",
			input:       "9223372036854775808:newest",
			expectedErr: `anchor offset "9223372036854775808" is too large`,
		},
		{
			testName:    "ending-offset-too-big",
			input:       "oldest:9223372036854775808",
			expectedErr: `anchor offset "9223372036854775808" is too large`,
		},
		{
			testName:    "partition-too-big",
			input:       "2147483648=oldest",
			expectedErr: `partition number "2147483648" is too large`,
		},
		{
			testName: "time-anchor-rfc3339",
			input:    "[2019-08-31T13:06:08.234Z]",
			expected: map[int32]interval{
				-1: {
					start: position{
						startIsTime: true,
						startTime: timeRange{
							t0: T("2019-08-31T13:06:08.234Z"),
							t1: T("2019-08-31T13:06:08.234Z"),
						},
					},
					end: position{
						startOffset: maxOffset,
					},
				},
			},
		},
		{
			testName: "time-anchor-rfc3339-not-utc",
			input:    "[2019-08-31T13:06:08.234-04:00]",
			expected: map[int32]interval{
				-1: {
					start: position{
						startIsTime: true,
						startTime: timeRange{
							t0: T("2019-08-31T17:06:08.234Z"),
							t1: T("2019-08-31T17:06:08.234Z"),
						},
					},
					end: position{
						startOffset: maxOffset,
					},
				},
			},
		},
		{
			testName: "time-anchor-date",
			input:    "[2019-08-31]",
			expected: map[int32]interval{
				-1: {
					start: position{
						startIsTime: true,
						startTime: timeRange{
							t0: T("2019-08-31T00:00:00Z"),
							t1: T("2019-09-01T00:00:00Z"),
						},
					},
					end: position{
						startOffset: maxOffset,
					},
				},
			},
		},
		{
			testName: "time-anchor-month",
			input:    "[2019-08]",
			expected: map[int32]interval{
				-1: {
					start: position{
						startIsTime: true,
						startTime: timeRange{
							t0: T("2019-08-01T00:00:00Z"),
							t1: T("2019-09-01T00:00:00Z"),
						},
					},
					end: position{
						startOffset: maxOffset,
					},
				},
			},
		},
		{
			testName: "time-anchor-year",
			input:    "[2019]",
			expected: map[int32]interval{
				-1: {
					start: position{
						startIsTime: true,
						startTime: timeRange{
							t0: T("2019-01-01T00:00:00Z"),
							t1: T("2020-01-01T00:00:00Z"),
						},
					},
					end: position{
						startOffset: maxOffset,
					},
				},
			},
		},
		{
			testName: "time-anchor-minute",
			input:    "[13:45]",
			expected: map[int32]interval{
				-1: {
					start: position{
						startIsTime: true,
						startTime: timeRange{
							t0: T("2011-02-03T13:45:00Z"),
							t1: T("2011-02-03T13:46:00Z"),
						},
					},
					end: position{
						startOffset: maxOffset,
					},
				},
			},
		},
		{
			testName: "time-anchor-second",
			input:    "[13:45:12.345]",
			expected: map[int32]interval{
				-1: {
					start: position{
						startIsTime: true,
						startTime: timeRange{
							t0: T("2011-02-03T13:45:12.345Z"),
							t1: T("2011-02-03T13:45:12.345Z"),
						},
					},
					end: position{
						startOffset: maxOffset,
					},
				},
			},
		},
		{
			testName: "time-anchor-hour",
			input:    "[4pm]",
			expected: map[int32]interval{
				-1: {
					start: position{
						startIsTime: true,
						startTime: timeRange{
							t0: T("2011-02-03T16:00:00Z"),
							t1: T("2011-02-03T17:00:00Z"),
						},
					},
					end: position{
						startOffset: maxOffset,
					},
				},
			},
		},
		{
			testName: "time-range",
			input:    "[2019-08-31T13:06:08.234Z]:[2023-02-05T12:01:02.6789Z]",
			expected: map[int32]interval{
				-1: {
					start: position{
						startIsTime: true,
						startTime: timeRange{
							t0: T("2019-08-31T13:06:08.234Z"),
							t1: T("2019-08-31T13:06:08.234Z"),
						},
					},
					end: position{
						startIsTime: true,
						startTime: timeRange{
							t0: T("2023-02-05T12:01:02.6789Z"),
							t1: T("2023-02-05T12:01:02.6789Z"),
						},
					},
				},
			},
		},
		{
			testName: "time-anchor-with-diff-offset",
			input:    "[4pm]-123",
			expected: map[int32]interval{
				-1: {
					start: position{
						startIsTime: true,
						startTime: timeRange{
							t0: T("2011-02-03T16:00:00Z"),
							t1: T("2011-02-03T17:00:00Z"),
						},
						diffOffset: -123,
					},
					end: position{
						startOffset: maxOffset,
					},
				},
			},
		},
		{
			testName: "offset-anchor-with-negative-time-rel",
			input:    "1234-1h3s",
			expected: map[int32]interval{
				-1: {
					start: position{
						startOffset: 1234,
						diffIsTime:  true,
						diffTime:    -(time.Hour + 3*time.Second),
					},
					end: position{
						startOffset: maxOffset,
					},
				},
			},
		},
		{
			testName: "offset-anchor-with-positive-time-rel",
			input:    "1234+555ms",
			expected: map[int32]interval{
				-1: {
					start: position{
						startOffset: 1234,
						diffIsTime:  true,
						diffTime:    555 * time.Millisecond,
					},
					end: position{
						startOffset: maxOffset,
					},
				},
			},
		},
		{
			testName: "time-anchor-combines-with-time-rel",
			input:    "[3pm]+5s",
			expected: map[int32]interval{
				-1: {
					start: position{
						startIsTime: true,
						startTime: timeRange{
							t0: T("2011-02-03T15:00:05Z"),
							t1: T("2011-02-03T16:00:05Z"),
						},
					},
					end: position{
						startOffset: maxOffset,
					},
				},
			},
		},
		{
			testName:    "invalid-relative-position",
			input:       "[3pm]foo",
			expectedErr: `invalid relative position "foo"`,
		},
		{
			testName:    "invalid-relative-position",
			input:       "[3pmm]",
			expectedErr: `invalid timestamp "3pmm"`,
		},
		{
			testName:    "invalid-timestamp-no-closing-]",
			input:       `[3pm`,
			expectedErr: `no closing ] found in "\[3pm"`,
		},
		{
			testName:    "extra position in interval",
			input:       `0:1:2`,
			expectedErr: `invalid interval "0:1:2"`,
		},
	}
	c := qt.New(t)
	// Choose a reference date that's not UTC, so we can ensure
	// that the timezone-dependent logic works correctly.
	now := T("2011-02-03T16:05:06.500Z").In(time.FixedZone("UTC-8", -8*60*60))
	for _, d := range data {
		c.Run(d.testName, func(c *qt.C) {
			actual, err := parseOffsets(d.input, now)
			if d.expectedErr != "" {
				c.Assert(err, qt.ErrorMatches, d.expectedErr)
				return
			}
			c.Assert(err, qt.Equals, nil)
			c.Assert(actual, deepEquals, d.expected)
		})
	}
}

func TestFindPartitionsToConsume(t *testing.T) {
	data := []struct {
		topic    string
		offsets  map[int32]interval
		consumer tConsumer
		expected []int32
	}{
		{
			topic: "a",
			offsets: map[int32]interval{
				10: {
					start: position{startOffset: 2},
					end:   position{startOffset: 4},
				},
			},
			consumer: tConsumer{
				topics:              []string{"a"},
				topicsErr:           nil,
				partitions:          map[string][]int32{"a": []int32{0, 10}},
				partitionsErr:       map[string]error{"a": nil},
				consumePartition:    map[tConsumePartition]tPartitionConsumer{},
				consumePartitionErr: map[tConsumePartition]error{},
				closeErr:            nil,
			},
			expected: []int32{10},
		},
		{
			topic: "a",
			offsets: map[int32]interval{
				-1: {
					start: position{startOffset: 3},
					end:   position{startOffset: 41},
				},
			},
			consumer: tConsumer{
				topics:              []string{"a"},
				topicsErr:           nil,
				partitions:          map[string][]int32{"a": []int32{0, 10}},
				partitionsErr:       map[string]error{"a": nil},
				consumePartition:    map[tConsumePartition]tPartitionConsumer{},
				consumePartitionErr: map[tConsumePartition]error{},
				closeErr:            nil,
			},
			expected: []int32{0, 10},
		},
	}

	for _, d := range data {
		target := &consumeCmd{
			consumer: d.consumer,
			topic:    d.topic,
			offsets:  d.offsets,
		}
		actual := target.findPartitions()

		if !reflect.DeepEqual(actual, d.expected) {
			t.Errorf(
				`
Expected: %#v
Actual:   %#v
Input:    topic=%#v offsets=%#v
	`,
				d.expected,
				actual,
				d.topic,
				d.offsets,
			)
			return
		}
	}
}

func TestConsume(t *testing.T) {
	closer := make(chan struct{})
	messageChan := make(<-chan *sarama.ConsumerMessage)
	calls := make(chan tConsumePartition)
	consumer := tConsumer{
		consumePartition: map[tConsumePartition]tPartitionConsumer{
			tConsumePartition{"hans", 1, 1}: tPartitionConsumer{messages: messageChan},
			tConsumePartition{"hans", 2, 1}: tPartitionConsumer{messages: messageChan},
		},
		calls: calls,
	}
	partitions := []int32{1, 2}
	target := consumeCmd{consumer: consumer}
	target.topic = "hans"
	target.brokers = []string{"localhost:9092"}
	target.offsets = map[int32]interval{
		-1: interval{start: position{startOffset: 1}, end: position{startOffset: 5}},
	}

	go target.consume(partitions)
	defer close(closer)

	end := make(chan struct{})
	go func(c chan tConsumePartition, e chan struct{}) {
		actual := []tConsumePartition{}
		expected := []tConsumePartition{
			tConsumePartition{"hans", 1, 1},
			tConsumePartition{"hans", 2, 1},
		}
		for {
			select {
			case call := <-c:
				actual = append(actual, call)
				sort.Sort(ByPartitionOffset(actual))
				if reflect.DeepEqual(actual, expected) {
					e <- struct{}{}
					return
				}
				if len(actual) == len(expected) {
					t.Errorf(
						`Got expected number of calls, but they are different.
Expected: %#v
Actual:   %#v
`,
						expected,
						actual,
					)
				}
			case _, ok := <-e:
				if !ok {
					return
				}
			}
		}
	}(calls, end)

	select {
	case <-end:
	case <-time.After(1 * time.Second):
		t.Errorf("Did not receive calls to consume partitions before timeout.")
		close(end)
	}
}

type tConsumePartition struct {
	topic     string
	partition int32
	offset    int64
}

type ByPartitionOffset []tConsumePartition

func (a ByPartitionOffset) Len() int {
	return len(a)
}
func (a ByPartitionOffset) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}
func (a ByPartitionOffset) Less(i, j int) bool {
	return a[i].partition < a[j].partition || a[i].offset < a[j].offset
}

type tPartitionConsumer struct {
	closeErr            error
	highWaterMarkOffset int64
	messages            <-chan *sarama.ConsumerMessage
	errors              <-chan *sarama.ConsumerError
}

func (pc tPartitionConsumer) AsyncClose() {}
func (pc tPartitionConsumer) Close() error {
	return pc.closeErr
}
func (pc tPartitionConsumer) HighWaterMarkOffset() int64 {
	return pc.highWaterMarkOffset
}
func (pc tPartitionConsumer) Messages() <-chan *sarama.ConsumerMessage {
	return pc.messages
}
func (pc tPartitionConsumer) Errors() <-chan *sarama.ConsumerError {
	return pc.errors
}

type tConsumer struct {
	topics              []string
	topicsErr           error
	partitions          map[string][]int32
	partitionsErr       map[string]error
	consumePartition    map[tConsumePartition]tPartitionConsumer
	consumePartitionErr map[tConsumePartition]error
	closeErr            error
	calls               chan tConsumePartition
}

func (c tConsumer) Topics() ([]string, error) {
	return c.topics, c.topicsErr
}

func (c tConsumer) Partitions(topic string) ([]int32, error) {
	return c.partitions[topic], c.partitionsErr[topic]
}

func (c tConsumer) ConsumePartition(topic string, partition int32, offset int64) (sarama.PartitionConsumer, error) {
	cp := tConsumePartition{topic, partition, offset}
	c.calls <- cp
	return c.consumePartition[cp], c.consumePartitionErr[cp]
}

func (c tConsumer) Close() error {
	return c.closeErr
}

func (c tConsumer) HighWaterMarks() map[string]map[int32]int64 {
	return nil
}

func TestConsumeParseArgs(t *testing.T) {
	topic := "test-topic"
	givenBroker := "hans:9092"
	brokers := []string{givenBroker}

	os.Setenv("KT_TOPIC", topic)
	os.Setenv("KT_BROKERS", givenBroker)
	target := &consumeCmd{}

	target.parseArgs([]string{})
	if target.topic != topic ||
		!reflect.DeepEqual(target.brokers, brokers) {
		t.Errorf("Expected topic %#v and brokers %#v from env vars, got %#v.", topic, brokers, target)
		return
	}

	// default brokers to localhost:9092
	os.Setenv("KT_TOPIC", "")
	os.Setenv("KT_BROKERS", "")
	brokers = []string{"localhost:9092"}

	target.parseArgs([]string{"-topic", topic})
	if target.topic != topic ||
		!reflect.DeepEqual(target.brokers, brokers) {
		t.Errorf("Expected topic %#v and brokers %#v from env vars, got %#v.", topic, brokers, target)
		return
	}

	// command line arg wins
	os.Setenv("KT_TOPIC", "BLUBB")
	os.Setenv("KT_BROKERS", "BLABB")
	brokers = []string{givenBroker}

	target.parseArgs([]string{"-topic", topic, "-brokers", givenBroker})
	if target.topic != topic ||
		!reflect.DeepEqual(target.brokers, brokers) {
		t.Errorf("Expected topic %#v and brokers %#v from env vars, got %#v.", topic, brokers, target)
		return
	}
}

func T(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// deepEquals allows comparison of the unexported fields inside the
// value returned by parseOffsets.
var deepEquals = qt.CmpEquals(cmp.AllowUnexported(position{}, interval{}, timeRange{}))
