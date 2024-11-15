package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/viper"
	"github.com/valyala/fastjson"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
)

type CEP struct {
	CEP string `json:"cep"`
}

type Response struct {
	City  string  `json:"city"`
	TempC float64 `json:"temp_C"`
	TempF float64 `json:"temp_F"`
	TempK float64 `json:"temp_K"`
}

func initProvider(serviceName, collectorURL string) (func(context.Context) error, error) {
	ctx := context.Background()

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, collectorURL,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection to collector: %w", err)
	}

	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	bsp := sdktrace.NewBatchSpanProcessor(traceExporter)
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)
	otel.SetTracerProvider(tracerProvider)

	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tracerProvider.Shutdown, nil
}

func init() {
	viper.AutomaticEnv()
}

func main() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	shutdown, err := initProvider("service-b", viper.GetString("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err = shutdown(ctx); err != nil {
			log.Fatal("failed to shutdown TracerProvider: %w", err)
		}
	}()

	http.HandleFunc("/", getTemp)
	http.Handle("/metrics", promhttp.Handler())

	go func() {
		log.Println("Starting server on port", viper.GetString("HTTP_PORT"))
		if err := http.ListenAndServe(viper.GetString("HTTP_PORT"), nil); err != nil {
			log.Fatal(err)
		}
	}()

	select {
	case <-sigCh:
		log.Println("Shutting down gracefully, CTRL+C pressed...")
	case <-ctx.Done():
		log.Println("Shutting down due to other reason...")
	}

	_, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
}

func getTemp(w http.ResponseWriter, r *http.Request) {
	carrier := propagation.HeaderCarrier(r.Header)
	ctx := r.Context()
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)

	tracer := otel.Tracer("microservice-tracer")
	ctx, span := tracer.Start(ctx, "service-b")
	defer span.End()

	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	zip := r.URL.Query().Get("zip")
	apiKey := r.Header.Get("api_key")
	if apiKey == "" {
		http.Error(w, "api_key is required", http.StatusBadRequest)
		return
	}

	client := &http.Client{}
	location, err := getLocation(ctx, client, zip)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	res, err := getTempFromAPI(ctx, client, location, apiKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res.City = location
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(res)
}

func getLocation(ctx context.Context, client *http.Client, zip string) (string, error) {
	baseURL := "http://viacep.com.br/ws/%s/json/"
	URL := fmt.Sprintf(baseURL, zip)

	req, err := http.NewRequestWithContext(ctx, "GET", URL, nil)
	if err != nil {
		return "", err
	}

	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	r, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}

	location := fastjson.GetString(r, "localidade")
	if location == "" {
		return "", errors.New("can not find zipcode")
	}

	return location, nil
}

func getTempFromAPI(ctx context.Context, client *http.Client, location string, apiKey string) (Response, error) {
	weather := Response{}
	baseURL := "http://api.weatherapi.com/v1/current.json"
	u, err := url.Parse(baseURL)
	if err != nil {
		return Response{}, err
	}

	q := u.Query()
	q.Set("q", location)
	q.Set("key", apiKey)
	u.RawQuery = q.Encode()

	tracer := otel.Tracer("microservice-tracer")
	ctx, span := tracer.Start(ctx, "outgoing request to weatherapi")
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return Response{}, err
	}

	res, err := client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer res.Body.Close()

	r, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return Response{}, err
	}

	var p fastjson.Parser
	v, err := p.Parse(string(r))
	if err != nil {
		return Response{}, err
	}
	weather.TempC = v.GetFloat64("current", "temp_c")
	weather.TempF = v.GetFloat64("current", "temp_f")
	weather.TempK = weather.TempC + 273
	if weather.TempC == 0 {
		return Response{}, errors.New("error getting weather, invalid API key")
	}

	return weather, nil
}
