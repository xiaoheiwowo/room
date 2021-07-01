package service

import (
	"bytepower_room/base"
	"bytepower_room/base/log"
	"context"
	"io/ioutil"
	"net/http"
	"sync/atomic"

	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	HTTPHeaderContentType = "Content-Type"
	HTTPContentTypeJSON   = "application/json"
)

const CollectEventServiceName = "collect_event_service"

type CollectEventService struct {
	config                  *base.CollectEventServiceConfig
	eventBuffer             chan base.HashTagEvent
	eventCountInEventBuffer int64
	logger                  *log.Logger
	metric                  *base.MetricClient
	db                      *base.DBCluster
	wg                      sync.WaitGroup
	stopCh                  chan bool
	stop                    int32
	server                  *http.Server
}

func NewCollectEventService(config base.CollectEventServiceConfig, logger *log.Logger, metric *base.MetricClient, db *base.DBCluster) (*CollectEventService, error) {
	if err := config.Init(); err != nil {
		return nil, err
	}
	if logger == nil {
		return nil, errors.New("logger should not be nil")
	}
	if metric == nil {
		return nil, errors.New("metric should not be nil")
	}
	if db == nil {
		return nil, errors.New("db should not be nil")
	}
	service := &CollectEventService{
		config:                  &config,
		eventBuffer:             make(chan base.HashTagEvent, config.BufferLimit),
		eventCountInEventBuffer: 0,
		logger:                  logger,
		metric:                  metric,
		db:                      db,
		wg:                      sync.WaitGroup{},
		stopCh:                  make(chan bool),
		stop:                    0,
		server:                  nil,
	}
	logger.Info(fmt.Sprintf("new %s", CollectEventServiceName), log.String("config", fmt.Sprintf("%+v", config)))
	return service, nil
}

func (service *CollectEventService) Run() {
	service.wg.Add(1)
	go service.startServer()
	for i := 0; i < service.config.SaveEvent.WorkerCount; i++ {
		service.wg.Add(1)
		go service.saveEvents()
	}
	service.wg.Add(1)
	go service.mointor(service.config.MonitorInterval)

}

func (service *CollectEventService) startServer() {
	defer func() {
		service.logger.Info(fmt.Sprintf("stop %s server", CollectEventServiceName))
		service.wg.Done()
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("/events", service.postEventsHandler)
	service.server = &http.Server{
		Addr:         service.config.Server.URL,
		Handler:      mux,
		ReadTimeout:  time.Duration(service.config.Server.ReadTimeoutMS) * time.Millisecond,
		WriteTimeout: time.Duration(service.config.Server.WriteTimeoutMS) * time.Millisecond,
		IdleTimeout:  time.Duration(service.config.Server.IdleTimeoutMS) * time.Millisecond,
	}
	go func() {
		if err := service.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			service.recordError("listen_serve", err, nil)
		}
	}()

	<-service.stopCh
	if err := service.server.Close(); err != nil {
		service.recordError("close_server", err, nil)
	} else {
		service.logger.Info(fmt.Sprintf("close %s server success", CollectEventServiceName))
	}
}

func (service *CollectEventService) saveEvents() {
	defer func() {
		service.logger.Info(fmt.Sprintf("stop %s save events", CollectEventServiceName))
		service.wg.Done()
	}()
loop:
	for {
		select {
		case event, ok := <-service.eventBuffer:
			if !ok {
				break loop
			}
			atomic.AddInt64(&service.eventCountInEventBuffer, -1)
			if err := service.saveEvent(event); err != nil {
				service.recordError(
					"save_event", err,
					map[string]string{"event": event.String()},
				)
			}
		case <-service.stopCh:
			break loop
		}
	}
}

func (service *CollectEventService) saveEvent(event base.HashTagEvent) error {
	if err := event.Check(); err != nil {
		return err
	}
	config := service.config.SaveEvent
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.TimeoutMS)*time.Millisecond)
	defer cancel()
	retryInterval := time.Duration(config.RetryIntervalMS) * time.Millisecond
	for i := 0; i < config.RetryTimes; i++ {
		err := upsertHashTagKeysRecordByEvent(ctx, service.db, event, time.Now())
		if err != nil {
			if errors.Is(err, base.DBTxError) {
				service.recordError("save_event_retry", err, map[string]string{"event": event.String()})
				time.Sleep(retryInterval)
				continue
			}
			return err
		}
		break
	}
	return nil
}

func (service *CollectEventService) AddEvent(event base.HashTagEvent) error {
	defer func() {
		if r := recover(); r != nil {
			service.recordError("add_event_panic", fmt.Errorf("%+v", r), nil)
		}
	}()
	if err := event.Check(); err != nil {
		return err
	}
	select {
	case service.eventBuffer <- event:
		atomic.AddInt64(&service.eventCountInEventBuffer, 1)
		return nil
	default:
		return fmt.Errorf(
			"%s buffer is full with limit %d, event %s is discarded",
			CollectEventServiceName, service.config.BufferLimit, event.String())
	}
}

func (service *CollectEventService) AddEvents(events []base.HashTagEvent) error {
	for _, event := range events {
		if err := service.AddEvent(event); err != nil {
			return err
		}
	}
	return nil
}

func (service *CollectEventService) Stop() {
	if atomic.CompareAndSwapInt32(&service.stop, 0, 1) {
		close(service.stopCh)
	}
	service.wg.Wait()
	service.drainEvents()
}

func (service *CollectEventService) drainEvents() {
	close(service.eventBuffer)
	for event := range service.eventBuffer {
		err := service.saveEvent(event)
		if err != nil {
			service.recordError(
				"save_event", err,
				map[string]string{"event": event.String()},
			)
		}
	}
}

func (service *CollectEventService) mointor(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer func() {
		service.logger.Info(fmt.Sprintf("stop %s monitor", CollectEventServiceName))
		ticker.Stop()
		service.wg.Done()
	}()
	metricName := "event_in_buffer"
loop:
	for {
		select {
		case <-ticker.C:
			service.recordGauge(metricName, atomic.LoadInt64(&service.eventCountInEventBuffer))
		case <-service.stopCh:
			break loop
		}
	}
}

func (service *CollectEventService) recordGauge(metricName string, count int64) {
	recordName := fmt.Sprintf("%s.%s.total", CollectEventServiceName, metricName)
	service.logger.Info(recordName, log.Int64("count", count))
	service.recordGaugeMetric(metricName, count)
}

func (service *CollectEventService) recordGaugeMetric(metricName string, count int64) {
	recordName := fmt.Sprintf("%s.%s.total", CollectEventServiceName, metricName)
	service.metric.MetricGauge(recordName, count)
}

func (service *CollectEventService) recordError(reason string, err error, info map[string]string) {
	logPairs := make([]log.LogPair, 0)
	logPairs = append(logPairs, log.String("service", CollectEventServiceName))
	if reason != "" {
		logPairs = append(logPairs, log.String("reason", reason))
	}
	for key, value := range info {
		logPairs = append(logPairs, log.String(key, value))
	}
	if err != nil {
		logPairs = append(logPairs, log.Error(err))
	}
	service.logger.Error(fmt.Sprintf("%s error", CollectEventServiceName), logPairs...)

	errorMetricName := fmt.Sprintf("%s.error", CollectEventServiceName)
	service.metric.MetricIncrease(errorMetricName)
	specificErrorMetricName := fmt.Sprintf("%s.%s", errorMetricName, reason)
	service.metric.MetricIncrease(specificErrorMetricName)
}

func (service *CollectEventService) recordWriteResponseError(err error, body []byte) {
	failedReasonWriteToClient := "write_to_client"
	service.recordError(failedReasonWriteToClient, err, map[string]string{"body": string(body)})
}

func (service *CollectEventService) recordSuccessWithDuration(info string, duration time.Duration) {
	metricName := fmt.Sprintf("%s.success.%s", CollectEventServiceName, info)
	service.metric.MetricIncrease(metricName)
	if duration > time.Duration(0) {
		durationMetricName := fmt.Sprintf("%s.duration", metricName)
		service.metric.MetricTimeDuration(durationMetricName, duration)
	}
}

func (service *CollectEventService) recordSuccessWithCount(info string, count int) {
	metricName := fmt.Sprintf("%s.success.%s", CollectEventServiceName, info)
	service.metric.MetricCount(metricName, count)
}

type CollectEventsRequestBody struct {
	Events []base.HashTagEvent `json:"events"`
}

func (service *CollectEventService) postEventsHandler(writer http.ResponseWriter, request *http.Request) {
	startTime := time.Now()
	if request.Method != http.MethodPost {
		err := fmt.Errorf("method %s is not allowed", request.Method)
		service.recordError("method_not_allowed", err, nil)
		if err = writeErrorResponse(writer, http.StatusMethodNotAllowed, err); err != nil {
			service.recordWriteResponseError(err, []byte{})
		}
		return
	}
	body, err := ioutil.ReadAll(request.Body)
	if err != nil {
		service.recordError("read_body", err, nil)
		if err = writeErrorResponse(writer, http.StatusInternalServerError, err); err != nil {
			service.recordWriteResponseError(err, []byte{})
		}
		return
	}
	service.recordGaugeMetric("request_body", int64(len(body)))
	requestBodyStruct := CollectEventsRequestBody{}
	if err = json.Unmarshal(body, &requestBodyStruct); err != nil {
		service.recordError("unmarshal_body", err, map[string]string{"body": string(body)})
		if err = writeErrorResponse(writer, http.StatusBadRequest, err); err != nil {
			service.recordWriteResponseError(err, body)
		}
		return
	}
	events := requestBodyStruct.Events
	for _, event := range events {
		if err = event.Check(); err != nil {
			service.recordError("event_check", err, map[string]string{"event": event.String()})
			if err = writeErrorResponse(writer, http.StatusBadRequest, err); err != nil {
				service.recordWriteResponseError(err, body)
			}
			return
		}
	}

	err = service.AddEvents(events)
	if err != nil {
		service.recordError("add_event", err, map[string]string{"body": string(body)})
		if err = writeErrorResponse(writer, http.StatusInternalServerError, err); err != nil {
			service.recordWriteResponseError(err, body)
		}
		return
	}
	if err = writeSuccessResponse(writer, len(events)); err != nil {
		service.recordWriteResponseError(err, body)
	}
	service.recordSuccessWithDuration("add_event", time.Since(startTime))
	service.recordSuccessWithCount("add_event.events", len(events))
}

func writeErrorResponse(writer http.ResponseWriter, code int, err error) error {
	writer.Header().Set(HTTPHeaderContentType, HTTPContentTypeJSON)
	writer.WriteHeader(code)
	body := map[string]string{"error": err.Error()}
	bodyInBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = writer.Write(bodyInBytes)
	return err
}

func writeSuccessResponse(writer http.ResponseWriter, count int) error {
	writer.Header().Set(HTTPHeaderContentType, HTTPContentTypeJSON)
	writer.WriteHeader(http.StatusOK)
	body := map[string]int{"count": count}
	bodyInBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = writer.Write(bodyInBytes)
	return err
}
