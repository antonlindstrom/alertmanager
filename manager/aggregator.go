// Copyright 2013 Prometheus Team
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

package manager

import (
	"errors"
	"log"
	"time"
)

const (
	minimumRefreshPeriod = 5 * time.Minute
	notificationRetryPeriod = 1 * time.Minute
)

// AggregationRule creates and manages the scope for received events.
type AggregationRule struct {
	Filters Filters

	RepeatRate time.Duration
}

type AggregationInstance struct {
	Rule   *AggregationRule
	Event *Event

	// When was this AggregationInstance created?
	Created time.Time
	// When was the last refresh received into this AggregationInstance?
	LastRefreshed time.Time

	// When was the last successful notification sent out for this
	// AggregationInstance?
	lastNotificationSent time.Time
	// Timer used to trigger a notification retry/resend.
	notificationResendTimer *time.Timer
	// Timer used to trigger the deletion of the AggregationInstance after it
	// hasn't been refreshed for too long.
	expiryTimer *time.Timer
}

func (r *AggregationRule) Handles(e *Event) bool {
	return r.Filters.Handles(e)
}

func (r *AggregationInstance) Ingest(e *Event) {
	r.Event = e
	r.LastRefreshed = time.Now()

	r.expiryTimer.Reset(minimumRefreshPeriod)
}

func (r *AggregationInstance) SendNotification(s SummaryReceiver) {
	if time.Since(r.lastNotificationSent) < r.Rule.RepeatRate {
		return
	}

	err := s.Receive(&EventSummary{
		Rule:   r.Rule,
		Event: r.Event,
	})
	if err != nil {
		log.Printf("Error while sending notification: %s, retrying in %v", err, notificationRetryPeriod)
		r.resendNotificationAfter(notificationRetryPeriod)
		return
	}

	r.resendNotificationAfter(r.Rule.RepeatRate)
	r.lastNotificationSent = time.Now()
}

func (r *AggregationInstance) resendNotificationAfter(d time.Duration) {
	timer := time.AfterFunc(d, func() {
		r.SendNotification()
	})
}

type AggregationRules []*AggregationRule

type Aggregator struct {
	Rules      AggregationRules
	Aggregates map[EventFingerprint]*AggregationInstance

	aggRequests   chan *aggregateEventsRequest
	getAggregatesRequests chan *getAggregatesRequest
	removeAggregateRequests chan EventFingerprint
	rulesRequests chan *aggregatorResetRulesRequest
	closed        chan bool
}

func NewAggregator() *Aggregator {
	return &Aggregator{
		Aggregates: make(map[EventFingerprint]*AggregationInstance),

		aggRequests:   make(chan *aggregateEventsRequest),
		getAggregatesRequests:   make(chan *getAggregatesRequest),
		removeAggregateRequests:   make(chan EventFingerprint),
		rulesRequests: make(chan *aggregatorResetRulesRequest),
		closed:        make(chan bool),
	}
}

func (a *Aggregator) Close() {
	close(a.rulesRequests)
	close(a.aggRequests)
	close(a.getAggregatesRequests)
	close(a.removeAggregateRequests)

	<-a.closed
	close(a.closed)
}

type aggregateEventsResponse struct {
	Err error
}

type aggregateEventsRequest struct {
	Events Events

	Response chan *aggregateEventsResponse
}

type getAggregatesRequest struct {
	Response chan []*AggregationInstance
}

func (a *Aggregator) aggregate(req *aggregateEventsRequest, s SummaryReceiver) {
	if len(a.Rules) == 0 {
		req.Response <- &aggregateEventsResponse{
			Err: errors.New("No aggregation rules"),
		}
		close(req.Response)
		return
	}
	log.Println("aggregating", *req)
	for _, element := range req.Events {
		for _, r := range a.Rules {
			log.Println("Checking rule", r, r.Handles(element))
			if r.Handles(element) {
				fp := element.Fingerprint()
				aggregation, ok := a.Aggregates[fp]
				if !ok {
					expTimer := time.AfterFunc(minimumRefreshPeriod, func() {
						a.removeAggregateRequests <- fp
					})

					aggregation = &AggregationInstance{
						Rule: r,
						Created: time.Now(),
						expiryTimer: expTimer,
					}

					a.Aggregates[fp] = aggregation
				}

				aggregation.Ingest(element)
				aggregation.SendNotification(s)
				break
			}
		}
	}

	req.Response <- new(aggregateEventsResponse)
	close(req.Response)
}

type aggregatorResetRulesResponse struct{}

type aggregatorResetRulesRequest struct {
	Rules AggregationRules

	Response chan *aggregatorResetRulesResponse
}

func (a *Aggregator) replaceRules(r *aggregatorResetRulesRequest) {
	log.Println("Replacing", len(r.Rules), "aggregator rules...")
	a.Rules = r.Rules

	r.Response <- new(aggregatorResetRulesResponse)
	close(r.Response)
}

func (a *Aggregator) AlertAggregates() []*AggregationInstance {
	req := &getAggregatesRequest{
		Response: make(chan []*AggregationInstance),
	}

	a.getAggregatesRequests <- req

	result := <-req.Response

	return result
}

func (a *Aggregator) aggregates() []*AggregationInstance {
	aggs := make([]*AggregationInstance, 0, len(a.Aggregates))
	for _, agg := range a.Aggregates {
		aggs = append(aggs, agg)
	}
	return aggs
}

func (a *Aggregator) Receive(e Events) error {
	req := &aggregateEventsRequest{
		Events:   e,
		Response: make(chan *aggregateEventsResponse),
	}

	a.aggRequests <- req

	result := <-req.Response

	return result.Err
}

func (a *Aggregator) SetRules(r AggregationRules) error {
	req := &aggregatorResetRulesRequest{
		Rules:    r,
		Response: make(chan *aggregatorResetRulesResponse),
	}

	a.rulesRequests <- req

	_ = <-req.Response

	return nil
}

func (a *Aggregator) Dispatch(s SummaryReceiver) {
	t := time.NewTicker(time.Second)
	defer t.Stop()

	closed := 0

	for closed < 2 {
		select {
		case req, open := <-a.aggRequests:
			a.aggregate(req, s)

			if !open {
				closed++
			}

		case rules, open := <-a.rulesRequests:
			a.replaceRules(rules)

			if !open {
				closed++
			}

		case req := <-a.getAggregatesRequests:
			aggs := a.aggregates()
			req.Response <- aggs

		case fp := <-a.removeAggregateRequests:
			log.Println("Deleting expired aggregation instance", a)
			delete(a.Aggregates, fp)
		}
	}

	a.closed <- true
}