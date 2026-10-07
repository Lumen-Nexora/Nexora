package nexora

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

type WalletsResource struct{ client *Client }
type TransfersResource struct{ client *Client }
type FXResource struct{ client *Client }
type SchedulesResource struct{ client *Client }
type WebhooksResource struct{ client *Client }
type FeesResource struct{ client *Client }
type KeysResource struct{ client *Client }
type FiatResource struct{ client *Client }
type PaymentLinksResource struct{ client *Client }
type RefundsResource struct{ client *Client }

func (r *WalletsResource) Create(ctx context.Context, options ...RequestOptions) (CreateWalletResponse, error) {
	var result CreateWalletResponse
	err := r.client.request(ctx, http.MethodPost, "/wallets", nil, nil, &result, options...)
	return result, err
}

func (r *WalletsResource) GetBalances(ctx context.Context, walletID string, options ...RequestOptions) (GetBalancesResponse, error) {
	var result GetBalancesResponse
	err := r.client.request(ctx, http.MethodGet, "/wallets/"+url.PathEscape(walletID)+"/balances", nil, nil, &result, options...)
	return result, err
}

func (r *WalletsResource) CreateTrustline(ctx context.Context, walletID string, request CreateTrustlineRequest, options ...RequestOptions) (TrustlineResponse, error) {
	var result TrustlineResponse
	err := r.client.request(ctx, http.MethodPost, "/wallets/"+url.PathEscape(walletID)+"/trustlines", request, nil, &result, options...)
	return result, err
}

func (r *TransfersResource) Create(ctx context.Context, request CreateTransferRequest, options ...RequestOptions) (TransferResponse, error) {
	var result TransferResponse
	err := r.client.request(ctx, http.MethodPost, "/transfers", request, nil, &result, options...)
	return result, err
}

func (r *TransfersResource) Get(ctx context.Context, transferID string, options ...RequestOptions) (TransferResponse, error) {
	var result TransferResponse
	err := r.client.request(ctx, http.MethodGet, "/transfers/"+url.PathEscape(transferID), nil, nil, &result, options...)
	return result, err
}

func (r *TransfersResource) List(ctx context.Context, query ListTransactionsQuery, options ...RequestOptions) (ListTransactionsResponse, error) {
	params := url.Values{"wallet_id": {query.WalletID}}
	if query.Limit != nil {
		params.Set("limit", strconv.FormatInt(*query.Limit, 10))
	}
	if query.Offset != nil {
		params.Set("offset", strconv.FormatInt(*query.Offset, 10))
	}
	var result ListTransactionsResponse
	err := r.client.request(ctx, http.MethodGet, "/transfers", nil, params, &result, options...)
	return result, err
}

func (r *TransfersResource) CreateBatch(ctx context.Context, request CreateBatchRequest, options ...RequestOptions) (BatchResponse, error) {
	var result BatchResponse
	err := r.client.request(ctx, http.MethodPost, "/transfers/batch", request, nil, &result, options...)
	return result, err
}

func (r *TransfersResource) GetBatch(ctx context.Context, batchID string, options ...RequestOptions) (BatchResponse, error) {
	var result BatchResponse
	err := r.client.request(ctx, http.MethodGet, "/transfers/batch/"+url.PathEscape(batchID), nil, nil, &result, options...)
	return result, err
}

func (r *TransfersResource) ExportBatch(ctx context.Context, batchID string, options ...RequestOptions) (string, error) {
	var result string
	err := r.client.request(ctx, http.MethodGet, "/transfers/batch/"+url.PathEscape(batchID)+"/export", nil, nil, &result, options...)
	return result, err
}

type ListTransactionsQuery struct {
	WalletID string
	Limit    *int64
	Offset   *int64
}

func (r *PaymentLinksResource) Create(ctx context.Context, request CreatePaymentLinkRequest, options ...RequestOptions) (PaymentLink, error) {
	var result PaymentLink
	err := r.client.request(ctx, http.MethodPost, "/payment-links", request, nil, &result, options...)
	return result, err
}

func (r *PaymentLinksResource) List(ctx context.Context, options ...RequestOptions) (PaymentLinksResponse, error) {
	var result PaymentLinksResponse
	err := r.client.request(ctx, http.MethodGet, "/payment-links", nil, nil, &result, options...)
	return result, err
}

func (r *PaymentLinksResource) Get(ctx context.Context, id string, options ...RequestOptions) (PaymentLink, error) {
	var result PaymentLink
	err := r.client.request(ctx, http.MethodGet, "/payment-links/"+url.PathEscape(id), nil, nil, &result, options...)
	return result, err
}

func (r *PaymentLinksResource) Cancel(ctx context.Context, id string, options ...RequestOptions) error {
	return r.client.request(ctx, http.MethodDelete, "/payment-links/"+url.PathEscape(id), nil, nil, nil, options...)
}

func (r *RefundsResource) Create(ctx context.Context, request CreateRefundRequest, options ...RequestOptions) (Refund, error) {
	var result Refund
	err := r.client.request(ctx, http.MethodPost, "/refunds", request, nil, &result, options...)
	return result, err
}

func (r *RefundsResource) Get(ctx context.Context, id string, options ...RequestOptions) (Refund, error) {
	var result Refund
	err := r.client.request(ctx, http.MethodGet, "/refunds/"+url.PathEscape(id), nil, nil, &result, options...)
	return result, err
}

func (r *RefundsResource) List(ctx context.Context, originalID string, options ...RequestOptions) (RefundsResponse, error) {
	var result RefundsResponse
	params := url.Values{"original_transaction_id": {originalID}}
	err := r.client.request(ctx, http.MethodGet, "/refunds", nil, params, &result, options...)
	return result, err
}

func (r *FXResource) Quote(ctx context.Context, request QuoteRequest, options ...RequestOptions) (QuoteResponse, error) {
	var result QuoteResponse
	err := r.client.request(ctx, http.MethodPost, "/fx/quote", request, nil, &result, options...)
	return result, err
}

func (r *FXResource) Convert(ctx context.Context, request ConvertRequest, options ...RequestOptions) (ConversionResponse, error) {
	var result ConversionResponse
	err := r.client.request(ctx, http.MethodPost, "/fx/convert", request, nil, &result, options...)
	return result, err
}

func (r *FXResource) GetRates(ctx context.Context, from, to string, options ...RequestOptions) (RateResponse, error) {
	var result RateResponse
	err := r.client.request(ctx, http.MethodGet, "/fx/rates", nil, url.Values{"from": {from}, "to": {to}}, &result, options...)
	return result, err
}

func (r *SchedulesResource) Create(ctx context.Context, request CreateScheduleRequest, options ...RequestOptions) (ScheduleResponse, error) {
	var result ScheduleResponse
	err := r.client.request(ctx, http.MethodPost, "/schedules", request, nil, &result, options...)
	return result, err
}

func (r *SchedulesResource) List(ctx context.Context, options ...RequestOptions) (ListSchedulesResponse, error) {
	var result ListSchedulesResponse
	err := r.client.request(ctx, http.MethodGet, "/schedules", nil, nil, &result, options...)
	return result, err
}

func (r *SchedulesResource) Update(ctx context.Context, scheduleID string, request UpdateScheduleRequest, options ...RequestOptions) (ScheduleResponse, error) {
	var result ScheduleResponse
	err := r.client.request(ctx, http.MethodPatch, "/schedules/"+url.PathEscape(scheduleID), request, nil, &result, options...)
	return result, err
}

func (r *SchedulesResource) Delete(ctx context.Context, scheduleID string, options ...RequestOptions) error {
	return r.client.request(ctx, http.MethodDelete, "/schedules/"+url.PathEscape(scheduleID), nil, nil, nil, options...)
}

func (r *WebhooksResource) Create(ctx context.Context, request RegisterWebhookRequest, options ...RequestOptions) (WebhookEndpointResponse, error) {
	var result WebhookEndpointResponse
	err := r.client.request(ctx, http.MethodPost, "/webhooks", request, nil, &result, options...)
	return result, err
}

func (r *WebhooksResource) List(ctx context.Context, options ...RequestOptions) (ListWebhooksResponse, error) {
	var result ListWebhooksResponse
	err := r.client.request(ctx, http.MethodGet, "/webhooks", nil, nil, &result, options...)
	return result, err
}

func (r *WebhooksResource) Delete(ctx context.Context, webhookID string, options ...RequestOptions) error {
	return r.client.request(ctx, http.MethodDelete, "/webhooks/"+url.PathEscape(webhookID), nil, nil, nil, options...)
}

func (r *WebhooksResource) GetDeliveries(ctx context.Context, webhookID string, limit, offset *int64, options ...RequestOptions) (ListDeliveriesResponse, error) {
	params := make(url.Values)
	if limit != nil {
		params.Set("limit", strconv.FormatInt(*limit, 10))
	}
	if offset != nil {
		params.Set("offset", strconv.FormatInt(*offset, 10))
	}
	var result ListDeliveriesResponse
	err := r.client.request(ctx, http.MethodGet, "/webhooks/"+url.PathEscape(webhookID)+"/deliveries", nil, params, &result, options...)
	return result, err
}

func (r *FeesResource) Get(ctx context.Context, options ...RequestOptions) (FeeScheduleResponse, error) {
	var result FeeScheduleResponse
	err := r.client.request(ctx, http.MethodGet, "/fees", nil, nil, &result, options...)
	return result, err
}

func (r *FeesResource) ListCollected(ctx context.Context, startDate, endDate string, options ...RequestOptions) (ListCollectedResponse, error) {
	params := make(url.Values)
	if startDate != "" {
		params.Set("start_date", startDate)
	}
	if endDate != "" {
		params.Set("end_date", endDate)
	}
	var result ListCollectedResponse
	err := r.client.request(ctx, http.MethodGet, "/admin/fees/collected", nil, params, &result, options...)
	return result, err
}

func (r *KeysResource) Create(ctx context.Context, request CreateKeyRequest, options ...RequestOptions) (APIKeyCreated, error) {
	var result APIKeyCreated
	err := r.client.request(ctx, http.MethodPost, "/keys", request, nil, &result, options...)
	return result, err
}

func (r *KeysResource) List(ctx context.Context, options ...RequestOptions) ([]APIKeySummary, error) {
	var result []APIKeySummary
	err := r.client.request(ctx, http.MethodGet, "/keys", nil, nil, &result, options...)
	return result, err
}

func (r *KeysResource) Delete(ctx context.Context, keyID string, options ...RequestOptions) error {
	return r.client.request(ctx, http.MethodDelete, "/keys/"+url.PathEscape(keyID), nil, nil, nil, options...)
}

func (r *FiatResource) Deposit(ctx context.Context, walletID string, request FiatDepositRequest, options ...RequestOptions) (DepositResponse, error) {
	var result DepositResponse
	err := r.client.request(ctx, http.MethodPost, "/wallets/"+url.PathEscape(walletID)+"/deposit/fiat", request, nil, &result, options...)
	return result, err
}

func (r *FiatResource) Withdraw(ctx context.Context, walletID string, request FiatWithdrawalRequest, options ...RequestOptions) (WithdrawalResponse, error) {
	var result WithdrawalResponse
	err := r.client.request(ctx, http.MethodPost, "/wallets/"+url.PathEscape(walletID)+"/withdraw/fiat", request, nil, &result, options...)
	return result, err
}
