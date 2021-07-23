package otellambda

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"

	"go.opentelemetry.io/contrib/propagators/aws/xray"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-lambda-go/otellambda"
)

var errorLogger = log.New(log.Writer(), "OTel Lambda Error: ", 0)

type Flusher interface {
	ForceFlush(context.Context) error
}

type noopFlusher struct{}

func (*noopFlusher) ForceFlush(context.Context) error{return nil}

// Compile time check our noopFlusher implements FLusher
var _ Flusher = &noopFlusher{}

type InstrumentationOption func(o *InstrumentationOptions)

type InstrumentationOptions struct {
	// TracerProvider is the TracerProvider which will be used
	// to create instrumentation spans
	// The default value of TracerProvider the global otel TracerProvider
	// returned by otel.GetTracerProvider()
	TracerProvider trace.TracerProvider

	// Flusher is the mechanism used to flush any unexported spans
	// each Lambda Invocation to avoid spans being unexported for long
	// when periods of time if Lambda freezes the execution environment
	// The default value of Flusher is a noop Flusher, using this
	// default can result in long data delays in asynchronous settings
	Flusher Flusher
}

var configuration InstrumentationOptions

func errorHandler(e error) func(context.Context, interface{}) (interface{}, error) {
	return func(context.Context, interface{}) (interface{}, error) {
		return nil, e
	}
}

// Ensure handler takes 0-2 values, with context
// as its first value if two arguments exist
func validateArguments(handler reflect.Type) (bool, error) {
	handlerTakesContext := false
	if handler.NumIn() > 2 {
		return false, fmt.Errorf("handlers may not take more than two arguments, but handler takes %d", handler.NumIn())
	} else if handler.NumIn() > 0 {
		contextType := reflect.TypeOf((*context.Context)(nil)).Elem()
		argumentType := handler.In(0)
		handlerTakesContext = argumentType.Implements(contextType)
		if handler.NumIn() > 1 && !handlerTakesContext {
			return false, fmt.Errorf("handler takes two arguments, but the first is not Context. got %s", argumentType.Kind())
		}
	}

	return handlerTakesContext, nil
}

// Ensure handler returns 0-2 values, with an error
// as its first value if any exist
func validateReturns(handler reflect.Type) error {
	errorType := reflect.TypeOf((*error)(nil)).Elem()

	switch n := handler.NumOut(); {
	case n > 2:
		return fmt.Errorf("handler may not return more than two values")
	case n > 1:
		if !handler.Out(1).Implements(errorType) {
			return fmt.Errorf("handler returns two values, but the second does not implement error")
		}
	case n == 1:
		if !handler.Out(0).Implements(errorType) {
			return fmt.Errorf("handler returns a single value, but it does not implement error")
		}
	}

	return nil
}

// Wraps and calls customer lambda handler then unpacks response as necessary
func wrapperInternals(handlerFunc interface{}, event reflect.Value, ctx context.Context, takesContext bool) (interface{}, error) {
	wrappedLambdaHandler := reflect.ValueOf(wrapper(handlerFunc))

	argsWrapped := []reflect.Value{reflect.ValueOf(ctx), event, reflect.ValueOf(takesContext)}
	response := wrappedLambdaHandler.Call(argsWrapped)[0].Interface().([]reflect.Value)

	// convert return values into (interface{}, error)
	var err error
	if len(response) > 0 {
		if errVal, ok := response[len(response)-1].Interface().(error); ok {
			err = errVal
		}
	}
	var val interface{}
	if len(response) > 1 {
		val = response[0].Interface()
	}

	return val, err
}

// converts the given payload to the correct event type
func payloadToEvent(eventType reflect.Type, payload interface{}) (reflect.Value, error) {
	event := reflect.New(eventType)

	// lambda SDK normally unmarshalls to customer event type, however
	// with the wrapper the SDK unmarshalls to map[string]interface{}
	// due to our use of reflection. Therefore we must convert this map
	// to customer's desired event, we do so by simply re-marshalling then
	// unmarshalling to the desired event type
	remarshalledPayload, err := json.Marshal(payload)
	if err != nil {
		return reflect.Value{}, err
	}

	if err := json.Unmarshal(remarshalledPayload, event.Interface()); err != nil {
		return reflect.Value{}, err
	}
	return event, nil
}

// LambdaHandlerWrapper Provides a lambda handler which wraps customer lambda handler with OTel Tracing
func LambdaHandlerWrapper(handlerFunc interface{}, options ...InstrumentationOption) interface{} {
	o := InstrumentationOptions{
		TracerProvider: otel.GetTracerProvider(),
		Flusher:       &noopFlusher{},
	}
	for _, opt := range options {
		opt(&o)
	}
	configuration = o

	if handlerFunc == nil {
		return errorHandler(fmt.Errorf("handler is nil"))
	}
	handlerType := reflect.TypeOf(handlerFunc)
	if handlerType.Kind() != reflect.Func {
		return errorHandler(fmt.Errorf("handler kind %s is not %s", handlerType.Kind(), reflect.Func))
	}

	takesContext, err := validateArguments(handlerType)
	if err != nil {
		return errorHandler(err)
	}

	if err := validateReturns(handlerType); err != nil {
		return errorHandler(err)
	}

	// note we will always take context to capture lambda context,
	// regardless of whether customer takes context
	if handlerType.NumIn() == 0 || handlerType.NumIn() == 1 && takesContext {
		return func(ctx context.Context) (interface{}, error) {
			var temp *interface{}
			event := reflect.ValueOf(temp)
			return wrapperInternals(handlerFunc, event, ctx, takesContext)
		}
	} else { // customer either takes both context and payload or just payload
		return func(ctx context.Context, payload interface{}) (interface{}, error) {
			event, err := payloadToEvent(handlerType.In(handlerType.NumIn()-1), payload)
			if err != nil {
				return nil, err
			}
			return wrapperInternals(handlerFunc, event.Elem(), ctx, takesContext)
		}
	}
}

// basic implementation of TextMapCarrier
// which wraps the default map type
type mapCarrier map[string]string

// Compile time check our mapCarrier implements propagation.TextMapCarrier
var _ propagation.TextMapCarrier = mapCarrier{}

// Get returns the value associated with the passed key.
func (mc mapCarrier) Get(key string) string {
	return mc[key]
}

// Set stores the key-value pair.
func (mc mapCarrier) Set(key string, value string) {
	mc[key] = value
}

// Keys lists the keys stored in this carrier.
func (mc mapCarrier) Keys() []string {
	keys := make([]string, 0, len(mc))
	for k := range mc {
		keys = append(keys, k)
	}
	return keys
}

// Adds OTel span surrounding customer handler call
func wrapper(handlerFunc interface{}) func(ctx context.Context, event interface{}, takesContext bool) []reflect.Value {
	return func(ctx context.Context, event interface{}, takesContext bool) []reflect.Value {

		ctx, span := tracingBegin(ctx)
		defer tracingEnd(ctx, span)

		handler := reflect.ValueOf(handlerFunc)
		var args []reflect.Value
		if takesContext {
			args = append(args, reflect.ValueOf(ctx))
		}
		if eventExists(event) {
			args = append(args, reflect.ValueOf(event))
		}

		response := handler.Call(args)

		return response
	}
}

// Determine if an interface{} is nil or the
// if the reflect.Value of the event is nil
func eventExists(event interface{}) bool {
	if event == nil {
		return false
	}

	// reflect.Value.isNil() can only be called on
	// Values of certain Kinds. Unsupported Kinds
	// will panic rather than return false
	switch reflect.TypeOf(event).Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Ptr, reflect.UnsafePointer, reflect.Interface, reflect.Slice:
		return !reflect.ValueOf(event).IsNil()
	}
	return true
}

type wrappedHandler struct {
	handler lambda.Handler
}

// Compile time check our Handler implements lambda.Handler
var _ lambda.Handler = wrappedHandler{}

// Invoke adds OTel span surrounding customer Handler invocation
func (h wrappedHandler) Invoke(ctx context.Context, payload []byte) ([]byte, error) {

	ctx, span := tracingBegin(ctx)
	defer tracingEnd(ctx, span)

	response, err := h.handler.Invoke(ctx, payload)
	if err != nil {
		return nil, err
	}

	return response, nil
}

// HandlerWrapper Provides a Handler which wraps customer Handler with OTel Tracing
func HandlerWrapper(handler lambda.Handler, options ...InstrumentationOption) lambda.Handler {
	o := InstrumentationOptions{
		TracerProvider: otel.GetTracerProvider(),
		Flusher:       &noopFlusher{},
	}
	for _, opt := range options {
		opt(&o)
	}
	configuration = o

	return wrappedHandler{handler: handler}
}

// Logic to start OTel Tracing
func tracingBegin(ctx context.Context) (context.Context, trace.Span) {
	// Add trace id to context
	xrayTraceId := os.Getenv("_X_AMZN_TRACE_ID")
	mc := mapCarrier{}
	mc.Set("X-Amzn-Trace-Id", xrayTraceId)
	propagator := xray.Propagator{}
	ctx = propagator.Extract(ctx, mc)

	// Get a named tracer with package path as its name.
	tracer := configuration.TracerProvider.Tracer(tracerName)

	var span trace.Span
	spanName := os.Getenv("AWS_LAMBDA_FUNCTION_NAME")

	var attributes []attribute.KeyValue
	lc, ok := lambdacontext.FromContext(ctx)
	if !ok {
		errorLogger.Println("failed to load lambda context from context, ensure tracing enabled in Lambda")
	}
	if lc != nil {
		ctxRequestID := lc.AwsRequestID
		attributes = append(attributes, attribute.KeyValue{Key: semconv.FaaSExecutionKey, Value: attribute.StringValue(ctxRequestID)})

		// Resource attrs added as span attr due to static tp
		// being created without meaningful context
		ctxFunctionArn := lc.InvokedFunctionArn
		attributes = append(attributes, attribute.KeyValue{Key: semconv.FaaSIDKey, Value: attribute.StringValue(ctxFunctionArn)})
		arnParts := strings.Split(ctxFunctionArn, ":")
		if len(arnParts) >= 5 {
			attributes = append(attributes, attribute.KeyValue{Key: semconv.CloudAccountIDKey, Value: attribute.StringValue(arnParts[4])})
		}
	}

	ctx, span = tracer.Start(ctx, spanName, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attributes...))

	return ctx, span
}

// Logic to wrap up OTel Tracing
func tracingEnd(ctx context.Context, span trace.Span) {
	span.End()

	// force flush any tracing data since lambda may freeze
	err := configuration.Flusher.ForceFlush(ctx)
	if err != nil {
		errorLogger.Println("failed to force a flush, lambda may freeze before instrumentation exported: ", err)
	}
}

func WithTracerProvider(tracerProvider trace.TracerProvider) InstrumentationOption {
	return func(o *InstrumentationOptions) {
		o.TracerProvider = tracerProvider
	}
}

func WithFlusher(flusher Flusher) InstrumentationOption {
	return func(o *InstrumentationOptions) {
		o.Flusher = flusher
	}
}