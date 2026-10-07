package nexora

type CreateWalletResponse = WalletCreated
type GetBalancesResponse = WalletBalances
type QuoteResponse = Quote
type ListTransactionsResponse struct {
	Transactions []TransferResponse `json:"transactions"`
}
type ListSchedulesResponse struct {
	Schedules []ScheduleResponse `json:"schedules"`
}
type ListWebhooksResponse struct {
	Endpoints []WebhookEndpointSummary `json:"endpoints"`
}
type ListDeliveriesResponse struct {
	Deliveries []WebhookDelivery `json:"deliveries"`
}
type ListCollectedResponse struct {
	Summary []FeeCollectionSummary `json:"summary"`
}
