package l7_req

import (
	"bytes"
	"context"
	"os"
	"unsafe"

	"github.com/ddosify/alaz/log"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/rlimit"
)

// match with values in l7_req.c
const (
	BPF_L7_PROTOCOL_UNKNOWN = iota
	BPF_L7_PROTOCOL_HTTP
	BPF_L7_PROTOCOL_AMQP
	BPF_L7_PROTOCOL_POSTGRES
)

// for user space
const (
	L7_PROTOCOL_HTTP     = "HTTP"
	L7_PROTOCOL_AMQP     = "AMQP"
	L7_PROTOCOL_POSTGRES = "POSTGRES"
	L7_PROTOCOL_UNKNOWN  = "UNKNOWN"
)

// Custom type for the enumeration
type L7ProtocolConversion uint32

// String representation of the enumeration values
func (e L7ProtocolConversion) String() string {
	switch e {
	case BPF_L7_PROTOCOL_HTTP:
		return L7_PROTOCOL_HTTP
	case BPF_L7_PROTOCOL_AMQP:
		return L7_PROTOCOL_AMQP
	case BPF_L7_PROTOCOL_POSTGRES:
		return L7_PROTOCOL_POSTGRES
	case BPF_L7_PROTOCOL_UNKNOWN:
		return L7_PROTOCOL_UNKNOWN
	default:
		return "Unknown"
	}
}

// match with values in l7_req.c, order is important
const (
	BPF_METHOD_UNKNOWN = iota
	BPF_METHOD_GET
	BPF_METHOD_POST
	BPF_METHOD_PUT
	BPF_METHOD_PATCH
	BPF_METHOD_DELETE
	BPF_METHOD_HEAD
	BPF_METHOD_CONNECT
	BPF_METHOD_OPTIONS
	BPF_METHOD_TRACE
)

// match with values in l7_req.c, order is important
const (
	BPF_AMQP_METHOD_UNKNOWN = iota
	BPF_AMQP_METHOD_PUBLISH
	BPF_AMQP_METHOD_DELIVER
)

// match with values in l7_req.c, order is important
const (
	BPF_POSTGRES_METHOD_UNKNOWN = iota
	BPF_POSTGRES_METHOD_STATEMENT_CLOSE_OR_CONN_TERMINATE
	BPF_POSTGRES_METHOD_SIMPLE_QUERY

	// BPF_POSTGRES_METHOD_QUERY
	// BPF_POSTGRES_METHOD_EXECUTE
	// BPF_POSTGRES_METHOD_PARSE
	// BPF_POSTGRES_METHOD_BIND
	// BPF_POSTGRES_METHOD_DESCRIBE
	// BPF_POSTGRES_METHOD_SYNC
	// BPF_POSTGRES_METHOD_FLUSH
	// BPF_POSTGRES_METHOD_CONSUME
	// BPF_POSTGRES_METHOD_PARSE_COMPLETE
	// BPF_POSTGRES_METHOD_BIND_COMPLETE
	// BPF_POSTGRES_METHOD_CLOSE_COMPLETE
	// BPF_POSTGRES_METHOD_SYNC_COMPLETE
	// BPF_POSTGRES_METHOD_READY_FOR_QUERY
	//...
)

// for http, user space
const (
	GET     = "GET"
	POST    = "POST"
	PUT     = "PUT"
	PATCH   = "PATCH"
	DELETE  = "DELETE"
	HEAD    = "HEAD"
	CONNECT = "CONNECT"
	OPTIONS = "OPTIONS"
	TRACE   = "TRACE"
)

// for rabbitmq, user space
const (
	PUBLISH = "PUBLISH"
	DELIVER = "DELIVER"
)

// for postgres, user space
const (
	CLOSE_OR_TERMINATE = "CLOSE_OR_TERMINATE"
	SIMPLE_QUERY       = "SIMPLE_QUERY"
)

// Custom type for the enumeration
type HTTPMethodConversion uint32

// String representation of the enumeration values
func (e HTTPMethodConversion) String() string {
	switch e {
	case BPF_METHOD_GET:
		return GET
	case BPF_METHOD_POST:
		return POST
	case BPF_METHOD_PUT:
		return PUT
	case BPF_METHOD_PATCH:
		return PATCH
	case BPF_METHOD_DELETE:
		return DELETE
	case BPF_METHOD_HEAD:
		return HEAD
	case BPF_METHOD_CONNECT:
		return CONNECT
	case BPF_METHOD_OPTIONS:
		return OPTIONS
	case BPF_METHOD_TRACE:
		return TRACE
	default:
		return "Unknown"
	}
}

// Custom type for the enumeration
type RabbitMQMethodConversion uint32

// String representation of the enumeration values
func (e RabbitMQMethodConversion) String() string {
	switch e {
	case BPF_AMQP_METHOD_PUBLISH:
		return PUBLISH
	case BPF_AMQP_METHOD_DELIVER:
		return DELIVER
	default:
		return "Unknown"
	}
}

// Custom type for the enumeration
type PostgresMethodConversion uint32

// String representation of the enumeration values
func (e PostgresMethodConversion) String() string {
	switch e {
	case BPF_POSTGRES_METHOD_STATEMENT_CLOSE_OR_CONN_TERMINATE:
		return CLOSE_OR_TERMINATE
	case BPF_POSTGRES_METHOD_SIMPLE_QUERY:
		return SIMPLE_QUERY
	default:
		return "Unknown"
	}
}

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS bpf l7.c -- -I../headers

const mapKey uint32 = 0

// for user space
type L7Event struct {
	Fd                  uint64
	Pid                 uint32
	Status              uint32
	Duration            uint64
	Protocol            string // L7_PROTOCOL_HTTP
	Tls                 bool   // Whether request was encrypted
	Method              string
	Payload             [512]uint8
	PayloadSize         uint32 // How much of the payload was copied
	PayloadReadComplete bool   // Whether the payload was copied completely
	Failed              bool   // Request failed
	WriteTimeNs         uint64 // start time of write syscall
}

const L7_EVENT = "l7_event"

func (e L7Event) Type() string {
	return L7_EVENT
}

var L7BpfProgsAndMaps bpfObjects

func LoadBpfObjects() {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Logger.Fatal().Err(err).Msg("failed to remove memlock limit")
	}
	// Load pre-compiled programs and maps into the kernel.
	L7BpfProgsAndMaps = bpfObjects{}
	if err := loadBpfObjects(&L7BpfProgsAndMaps, nil); err != nil {
		log.Logger.Fatal().Err(err).Msg("loading objects")
	}
}

// returns when program is detached
func DeployAndWait(parentCtx context.Context, ch chan interface{}) {
	ctx, _ := context.WithCancel(parentCtx)
	defer L7BpfProgsAndMaps.Close()

	// link programs
	l, err := link.Tracepoint("syscalls", "sys_enter_read", L7BpfProgsAndMaps.bpfPrograms.SysEnterRead, nil)
	if err != nil {
		log.Logger.Fatal().Err(err).Msg("link sys_enter_read tracepoint")
	}
	log.Logger.Info().Msg("sys_enter_read linked")
	defer func() {
		log.Logger.Info().Msg("closing sys_enter_read tracepoint")
		l.Close()
	}()

	l1, err := link.Tracepoint("syscalls", "sys_enter_write", L7BpfProgsAndMaps.bpfPrograms.SysEnterWrite, nil)
	if err != nil {
		log.Logger.Warn().Str("verifier log", string(L7BpfProgsAndMaps.bpfPrograms.SysEnterWrite.VerifierLog)).Msg("verifier log")
		log.Logger.Fatal().Err(err).Msg("link sys_enter_write tracepoint")
	}
	log.Logger.Info().Msg("sys_enter_write linked")
	defer func() {
		log.Logger.Info().Msg("closing sys_enter_write tracepoint")
		l1.Close()
	}()

	l2, err := link.Tracepoint("syscalls", "sys_exit_read", L7BpfProgsAndMaps.bpfPrograms.SysExitRead, nil)
	if err != nil {
		log.Logger.Fatal().Err(err).Msg("link sys_exit_read tracepoint")
	}
	log.Logger.Info().Msg("sys_exit_read linked")
	defer func() {
		log.Logger.Info().Msg("closing sys_exit_read tracepoint")
		l2.Close()
	}()

	l3, err := link.Tracepoint("syscalls", "sys_enter_sendto", L7BpfProgsAndMaps.bpfPrograms.SysEnterSendto, nil)
	if err != nil {
		log.Logger.Fatal().Err(err).Msg("link sys_enter_sendto tracepoint")
	}
	log.Logger.Info().Msg("sys_enter_sendto linked")
	defer func() {
		log.Logger.Info().Msg("closing sys_enter_sendto tracepoint")
		l3.Close()
	}()

	l4, err := link.Tracepoint("syscalls", "sys_enter_recvfrom", L7BpfProgsAndMaps.bpfPrograms.SysEnterRecvfrom, nil)
	if err != nil {
		log.Logger.Fatal().Err(err).Msg("link sys_enter_recvfrom tracepoint")
	}
	log.Logger.Info().Msg("sys_enter_recvfrom linked")
	defer func() {
		log.Logger.Info().Msg("closing sys_enter_recvfrom tracepoint")
		l4.Close()
	}()

	l5, err := link.Tracepoint("syscalls", "sys_exit_recvfrom", L7BpfProgsAndMaps.bpfPrograms.SysExitRecvfrom, nil)
	if err != nil {
		log.Logger.Fatal().Err(err).Msg("link sys_exit_recvfrom tracepoint")
	}
	log.Logger.Info().Msg("sys_exit_recvfrom linked")
	defer func() {
		log.Logger.Info().Msg("closing sys_exit_recvfrom tracepoint")
		l5.Close()
	}()

	l6, err := link.Tracepoint("syscalls", "sys_exit_sendto", L7BpfProgsAndMaps.bpfPrograms.SysExitSendto, nil)
	if err != nil {
		log.Logger.Fatal().Err(err).Msg("link sys_exit_sendto tracepoint")
	}
	log.Logger.Info().Msg("sys_exit_sendto linked")
	defer func() {
		log.Logger.Info().Msg("closing sys_exit_sendto tracepoint")
		l6.Close()
	}()

	l7, err := link.Tracepoint("syscalls", "sys_exit_write", L7BpfProgsAndMaps.bpfPrograms.SysExitWrite, nil)
	if err != nil {
		log.Logger.Fatal().Err(err).Msg("link sys_exit_write tracepoint")
	}
	log.Logger.Info().Msg("sys_exit_write linked")
	defer func() {
		log.Logger.Info().Msg("closing sys_exit_write tracepoint")
		l7.Close()
	}()

	// initialize perf event readers
	l7Events, err := perf.NewReader(L7BpfProgsAndMaps.L7Events, 64*os.Getpagesize())
	if err != nil {
		log.Logger.Fatal().Err(err).Msg("error creating perf event array reader")
	}
	defer func() {
		log.Logger.Info().Msg("closing l7 events perf event array reader")
		l7Events.Close()
	}()

	logs, err := perf.NewReader(L7BpfProgsAndMaps.LogMap, 64*os.Getpagesize())
	if err != nil {
		log.Logger.Fatal().Err(err).Msg("error creating perf event array reader")
	}
	defer func() {
		log.Logger.Info().Msg("closing l7 events perf event array reader")
		logs.Close()
	}()

	logsDone := make(chan struct{}, 1)
	readDone := make(chan struct{})

	go func() {
		var logMessage []byte
		var funcName []byte
		read := func() {
			record, err := logs.Read()
			if err != nil {
				log.Logger.Warn().Err(err).Msg("error reading from perf array")
			}

			if record.LostSamples != 0 {
				log.Logger.Debug().Msgf("lost #%d samples due to ring buffer's full", record.LostSamples)
			}

			if record.RawSample == nil || len(record.RawSample) == 0 {
				log.Logger.Debug().Msgf("read empty record from perf array")
				return
			}

			logMsg := (*bpfLogMessage)(unsafe.Pointer(&record.RawSample[0]))

			funcEnd := findEndIndex(logMsg.FuncName)
			msgEnd := findEndIndex(logMsg.LogMsg)

			logMessage = logMsg.LogMsg[:msgEnd]
			funcName = logMsg.FuncName[:funcEnd]

			args := []struct {
				argName  string
				argValue uint64
			}{
				{
					argName:  "",
					argValue: 0,
				},
				{
					argName:  "",
					argValue: 0,
				},
				{
					argName:  "",
					argValue: 0,
				},
			}

			parseLogMessage := func(input []byte, logMsg *bpfLogMessage) []byte {
				// fd,x,y -- {log-msg}
				// fd,, -- {log-msg}

				parts := bytes.SplitN(input, []byte(" -- "), 2)
				if len(parts) != 2 {
					log.Logger.Warn().Msgf("invalid ebpf log message: %s", string(input))
					return nil
				}

				parsedArgs := bytes.SplitN(parts[1], []byte("|"), 3)
				if len(parsedArgs) != 3 {
					log.Logger.Warn().Msgf("invalid ebpf log message not 3 args: %s", string(input))
					return nil
				}

				args[0].argName = string(parsedArgs[0])
				args[0].argValue = logMsg.Arg1

				args[1].argName = string(parsedArgs[1])
				args[1].argValue = logMsg.Arg2

				args[2].argName = string(parsedArgs[2])
				args[2].argValue = logMsg.Arg3
				return parts[0]
			}

			// will change resultArgs
			logMessage = parseLogMessage(logMessage, logMsg)
			if logMessage == nil {
				log.Logger.Warn().Msgf("invalid ebpf log message: %s", string(logMsg.LogMsg[:]))
				return
			}

			switch logMsg.Level {
			case 0:
				log.Logger.Debug().Str("func", string(funcName)).Uint32("pid", logMsg.Pid).
					Uint64(args[0].argName, args[0].argValue).Uint64(args[1].argName, args[1].argValue).Uint64(args[2].argName, args[2].argValue).
					Str("log-msg", string(logMessage)).Msg("ebpf-log")
			case 1:
				log.Logger.Info().Str("func", string(funcName)).Uint32("pid", logMsg.Pid).
					Uint64(args[0].argName, args[0].argValue).Uint64(args[1].argName, args[1].argValue).Uint64(args[2].argName, args[2].argValue).
					Str("log-msg", string(logMessage)).Msg("ebpf-log")
			case 2:
				log.Logger.Warn().Str("func", string(funcName)).Uint32("pid", logMsg.Pid).
					Uint64(args[0].argName, args[0].argValue).Uint64(args[1].argName, args[1].argValue).Uint64(args[2].argName, args[2].argValue).
					Str("log-msg", string(logMessage)).Msg("ebpf-log")
			case 3:
				log.Logger.Error().Str("func", string(funcName)).Uint32("pid", logMsg.Pid).
					Uint64(args[0].argName, args[0].argValue).Uint64(args[1].argName, args[1].argValue).Uint64(args[2].argName, args[2].argValue).
					Str("log-msg", string(logMessage)).Msg("ebpf-log")
			}
		}
		for {
			select {
			case <-logsDone:
				return
			default:
				read()
			}
		}
	}()

	go func() {
		read := func() {
			record, err := l7Events.Read()
			if err != nil {
				log.Logger.Warn().Err(err).Msg("error reading from perf array")
			}

			if record.LostSamples != 0 {
				log.Logger.Debug().Msgf("lost samples l7-event %d", record.LostSamples)
			}

			// TODO: investigate why this is happening
			if record.RawSample == nil || len(record.RawSample) == 0 {
				log.Logger.Debug().Msgf("read sample l7-event nil or empty")
				return
			}

			l7Event := (*bpfL7Event)(unsafe.Pointer(&record.RawSample[0]))

			go func() {

				protocol := L7ProtocolConversion(l7Event.Protocol).String()
				var method string
				switch protocol {
				case L7_PROTOCOL_HTTP:
					method = HTTPMethodConversion(l7Event.Method).String()
				case L7_PROTOCOL_AMQP:
					method = RabbitMQMethodConversion(l7Event.Method).String()
				case L7_PROTOCOL_POSTGRES:
					method = PostgresMethodConversion(l7Event.Method).String()
				default:
					method = "Unknown"
				}

				if uint8ToBool(l7Event.IsTls) {
					log.Logger.Debug().Uint16("fd", uint16(l7Event.Fd)).Uint32("pid", l7Event.Pid).
						Str("payload", string(l7Event.Payload[:])).Str("method", method).Str("protocol", protocol).Uint32("status", l7Event.Status).Msg("l7tls event")
				}

				ch <- L7Event{
					Fd:                  l7Event.Fd,
					Pid:                 l7Event.Pid,
					Status:              l7Event.Status,
					Duration:            l7Event.Duration,
					Protocol:            protocol,
					Tls:                 uint8ToBool(l7Event.IsTls),
					Method:              method,
					Payload:             l7Event.Payload,
					PayloadSize:         l7Event.PayloadSize,
					PayloadReadComplete: uint8ToBool(l7Event.PayloadReadComplete),
					Failed:              uint8ToBool(l7Event.Failed),
					WriteTimeNs:         l7Event.WriteTimeNs,
				}
			}()
		}
		for {
			select {
			case <-readDone:
				return
			default:
				read()
			}

		}
	}()

	<-ctx.Done() // wait for context to be cancelled
	readDone <- struct{}{}
	logsDone <- struct{}{}
	// defers will clean up
}

// 0 is false, 1 is true
func uint8ToBool(num uint8) bool {
	return num != 0
}

func findEndIndex(b [100]uint8) (endIndex int) {
	for i, v := range b {
		if v == 0 {
			return i
		}
	}
	return len(b)
}
