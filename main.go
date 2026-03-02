package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/codingconcepts/errhandler"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

type Server struct {
	db     *pgxpool.Pool
	client *openai.Client
	model  shared.ChatModel
}

type ChatRequest struct {
	SessionID  string `json:"session_id"`
	CustomerID string `json:"customer_id"`
	Message    string `json:"message"`
}

type ChatResponse struct {
	SessionID string `json:"session_id"`
	Reply     string `json:"reply"`
}

type sessionKey struct {
	CustomerID string
}

var sessions = map[sessionKey][]openai.ChatCompletionMessageParamUnion{}

func main() {
	ctx := context.Background()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	openAIKey := os.Getenv("OPENAI_API_KEY")
	if openAIKey == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	client := openai.NewClient(option.WithAPIKey(openAIKey))

	s := &Server{
		db:     pool,
		client: &client,
		model:  openai.ChatModelGPT5_2,
	}

	mux := http.NewServeMux()
	mux.Handle("POST /chat", errhandler.Wrap(s.handleChat))
	mux.Handle("/", http.FileServer(http.Dir("./static")))
	mux.Handle("POST /ui/chat", errhandler.Wrap(s.handleUIChat))

	addr := ":8080"
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (s *Server) handleUIChat(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := r.ParseForm(); err != nil {
		return errhandler.Error(http.StatusBadRequest, err)
	}

	customerID := strings.TrimSpace(r.FormValue("customer_id"))
	message := strings.TrimSpace(r.FormValue("message"))

	log.Printf("ui chat customer=%s msg=%q", customerID, message)

	if customerID == "" || message == "" {
		return errhandler.Error(http.StatusBadRequest, errors.New("missing customer_id or message"))
	}

	key := sessionKey{CustomerID: customerID}
	history := sessions[key]
	if len(history) == 0 {
		history = []openai.ChatCompletionMessageParamUnion{
			openai.DeveloperMessage(
				`You are a personal shopping assistant.
You can look up products, manage the user's basket, and place orders.
Be concise. When you need DB facts, call tools.`,
			),
		}
	}

	history = append(history, openai.UserMessage(message))
	reply, updated, err := s.runAgentLoop(ctx, customerID, history)
	if err != nil {
		return errhandler.Error(http.StatusInternalServerError, err)
	}
	sessions[key] = updated

	w.Header().Set("content-type", "text/html; charset=utf-8")

	response := fmt.Sprintf(`
<div class="msg">
  <div class="me">You</div>
  <div class="bubble">%s</div>
</div>
<div class="msg">
  <div class="bot">Agent</div>
  <div class="bubble">%s</div>
</div>
`, htmlEscape(message), htmlEscape(reply))

	return errhandler.SendString(w, response)
}

func htmlEscape(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return replacer.Replace(s)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) error {
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errhandler.Error(http.StatusBadRequest, err)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	key := sessionKey{CustomerID: req.CustomerID}
	history := sessions[key]
	if len(history) == 0 {
		history = []openai.ChatCompletionMessageParamUnion{
			openai.DeveloperMessage(
				`You are a personal shopping assistant.
You can look up products, manage the user's basket, and place orders.
Be concise. When you need DB facts, call tools. When something is ambiguous, ask a short follow-up question.`,
			),
		}
	}

	history = append(history, openai.UserMessage(req.Message))

	reply, updated, err := s.runAgentLoop(ctx, req.CustomerID, history)
	if err != nil {
		return errhandler.Error(http.StatusInternalServerError, err)
	}

	sessions[key] = updated

	return errhandler.SendJSON(w, ChatResponse{
		SessionID: req.SessionID,
		Reply:     reply,
	})
}

func (s *Server) runAgentLoop(ctx context.Context, shopperID string, messages []openai.ChatCompletionMessageParamUnion) (string, []openai.ChatCompletionMessageParamUnion, error) {
	tools := []openai.ChatCompletionToolUnionParam{
		openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        "search_products",
			Description: openai.String("Search products by (partial) name, returning id, name, price."),
			Parameters: shared.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
					"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "default": 10},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
		}),
		openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        "view_basket",
			Description: openai.String("Show the shopper's current basket contents with line totals and basket total."),
			Parameters: shared.FunctionParameters{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		}),
		openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        "add_to_basket",
			Description: openai.String("Add a product to basket by product_id (preferred) or by exact product_name. Increments quantity if already present."),
			Parameters: shared.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"product_id":   map[string]any{"type": "string", "description": "UUID"},
					"product_name": map[string]any{"type": "string", "description": "Exact product name if product_id not provided"},
					"quantity":     map[string]any{"type": "integer", "minimum": 1, "default": 1},
				},
				"additionalProperties": false,
			},
		}),
		openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        "remove_from_basket",
			Description: openai.String("Remove a product from basket by product_id (preferred) or by exact product_name."),
			Parameters: shared.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"product_id":   map[string]any{"type": "string", "description": "UUID"},
					"product_name": map[string]any{"type": "string", "description": "Exact product name if product_id not provided"},
				},
				"additionalProperties": false,
			},
		}),
		openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        "checkout",
			Description: openai.String("Create a purchase from basket (status=pending), create purchase_item rows, clear basket, and return purchase_id + total."),
			Parameters: shared.FunctionParameters{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		}),
		openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        "list_purchases",
			Description: openai.String("List recent purchases for the shopper, including id, total, status, timestamp."),
			Parameters: shared.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "default": 10},
				},
				"additionalProperties": false,
			},
		}),
		openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        "get_purchase_status",
			Description: openai.String("Get status for a purchase id."),
			Parameters: shared.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"purchase_id": map[string]any{"type": "string", "description": "UUID"},
				},
				"required":             []string{"purchase_id"},
				"additionalProperties": false,
			},
		}),
	}

	// Iteratively satisfy tool calls
	for steps := 0; steps < 8; steps++ {
		cc, err := s.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:    s.model,
			Messages: messages,
			Tools:    tools,
			// ToolChoice: openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String("auto")},
		})
		if err != nil {
			return "", messages, err
		}

		msg := cc.Choices[0].Message
		// Add assistant message to history (so the model can see its own tool call reasoning and outputs)
		messages = append(messages, msg.ToParam())

		toolCalls := msg.ToolCalls
		if len(toolCalls) == 0 {
			return msg.Content, messages, nil
		}

		for _, tc := range toolCalls {
			name := tc.Function.Name
			argsJSON := tc.Function.Arguments

			out, err := s.dispatchTool(ctx, shopperID, name, argsJSON)
			if err != nil {
				out = fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error())
			}

			messages = append(messages, openai.ToolMessage(out, tc.ID))
		}
	}

	return "", messages, errors.New("too many tool steps")
}

// ---- Tool dispatcher

func (s *Server) dispatchTool(ctx context.Context, shopperID string, name string, argsJSON string) (string, error) {
	switch name {
	case "search_products":
		var a struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		_ = json.Unmarshal([]byte(argsJSON), &a)
		if a.Limit <= 0 {
			a.Limit = 10
		}
		return s.toolSearchProducts(ctx, a.Query, a.Limit)

	case "view_basket":
		return s.toolViewBasket(ctx, shopperID)

	case "add_to_basket":
		var a struct {
			ProductID   string `json:"product_id"`
			ProductName string `json:"product_name"`
			Quantity    int    `json:"quantity"`
		}
		_ = json.Unmarshal([]byte(argsJSON), &a)
		if a.Quantity <= 0 {
			a.Quantity = 1
		}
		return s.toolAddToBasket(ctx, shopperID, a.ProductID, a.ProductName, a.Quantity)

	case "remove_from_basket":
		var a struct {
			ProductID   string `json:"product_id"`
			ProductName string `json:"product_name"`
		}
		_ = json.Unmarshal([]byte(argsJSON), &a)
		return s.toolRemoveFromBasket(ctx, shopperID, a.ProductID, a.ProductName)

	case "checkout":
		return s.toolCheckout(ctx, shopperID)

	case "list_purchases":
		var a struct {
			Limit int `json:"limit"`
		}
		_ = json.Unmarshal([]byte(argsJSON), &a)
		if a.Limit <= 0 {
			a.Limit = 10
		}
		return s.toolListPurchases(ctx, shopperID, a.Limit)

	case "get_purchase_status":
		var a struct {
			PurchaseID string `json:"purchase_id"`
		}
		_ = json.Unmarshal([]byte(argsJSON), &a)
		return s.toolGetPurchaseStatus(ctx, shopperID, a.PurchaseID)

	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func (s *Server) toolSearchProducts(ctx context.Context, query string, limit int) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return `{"ok":false,"error":"query is required"}`, nil
	}

	log.Println(query)

	rows, err := s.db.Query(ctx, `
		SELECT id, name, price
		FROM product
		WHERE name ILIKE '%' || $1 || '%'
		ORDER BY name
		LIMIT $2
	`, query, limit)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type P struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Price string `json:"price"`
	}
	var ps []P
	for rows.Next() {
		var p P
		if err := rows.Scan(&p.ID, &p.Name, &p.Price); err != nil {
			return "", err
		}
		ps = append(ps, p)
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "products": ps})
	return string(b), nil
}

func (s *Server) toolViewBasket(ctx context.Context, shopperID string) (string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT p.id, p.name, p.price, b.quantity, (p.price * b.quantity)::DECIMAL AS line_total
		FROM basket b
		JOIN product p ON p.id = b.product_id
		WHERE b.shopper_id = $1
		ORDER BY p.name
	`, shopperID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type Line struct {
		ProductID string `json:"product_id"`
		Name      string `json:"name"`
		Price     string `json:"price"`
		Quantity  int    `json:"quantity"`
		LineTotal string `json:"line_total"`
	}

	var lines []Line
	var totalStr string
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.ProductID, &l.Name, &l.Price, &l.Quantity, &l.LineTotal); err != nil {
			return "", err
		}
		lines = append(lines, l)
	}
	// total
	_ = s.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(p.price * b.quantity), 0)::DECIMAL
		FROM basket b JOIN product p ON p.id=b.product_id
		WHERE b.shopper_id=$1
	`, shopperID).Scan(&totalStr)

	b, _ := json.Marshal(map[string]any{"ok": true, "lines": lines, "total": totalStr})
	return string(b), nil
}

func (s *Server) toolAddToBasket(ctx context.Context, shopperID, productID, productName string, qty int) (string, error) {
	productID = strings.TrimSpace(productID)
	productName = strings.TrimSpace(productName)

	if productID == "" && productName == "" {
		return `{"ok":false,"error":"product_id or product_name is required"}`, nil
	}

	if productID == "" {
		// look up exact product name
		err := s.db.QueryRow(ctx, `SELECT id FROM product WHERE name=$1`, productName).Scan(&productID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return `{"ok":false,"error":"no product with that exact name; try search_products"}`, nil
			}
			return "", err
		}
	}

	_, err := s.db.Exec(ctx, `
		INSERT INTO basket (shopper_id, product_id, quantity)
		VALUES ($1, $2, $3)
		ON CONFLICT (shopper_id, product_id)
		DO UPDATE SET quantity = basket.quantity + EXCLUDED.quantity
	`, shopperID, productID, qty)
	if err != nil {
		return "", err
	}

	return `{"ok":true}`, nil
}

func (s *Server) toolRemoveFromBasket(ctx context.Context, shopperID, productID, productName string) (string, error) {
	productID = strings.TrimSpace(productID)
	productName = strings.TrimSpace(productName)

	if productID == "" && productName == "" {
		return `{"ok":false,"error":"product_id or product_name is required"}`, nil
	}
	if productID == "" {
		err := s.db.QueryRow(ctx, `SELECT id FROM product WHERE name=$1`, productName).Scan(&productID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return `{"ok":false,"error":"no product with that exact name"}`, nil
			}
			return "", err
		}
	}

	ct, err := s.db.Exec(ctx, `DELETE FROM basket WHERE shopper_id=$1 AND product_id=$2`, shopperID, productID)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "deleted_rows": ct.RowsAffected()})
	return string(b), nil
}

func (s *Server) toolCheckout(ctx context.Context, shopperID string) (string, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Compute total
	var total string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(p.price * b.quantity), 0)::DECIMAL
		FROM basket b JOIN product p ON p.id=b.product_id
		WHERE b.shopper_id=$1
	`, shopperID).Scan(&total); err != nil {
		return "", err
	}
	if total == "0" || total == "0.0" || total == "0.00" {
		return `{"ok":false,"error":"basket is empty"}`, nil
	}

	// Create purchase
	var purchaseID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO purchase (shopper_id, total, status)
		VALUES ($1, $2, 'pending')
		RETURNING id
	`, shopperID, total).Scan(&purchaseID); err != nil {
		return "", err
	}

	// Create purchase items from basket
	_, err = tx.Exec(ctx, `
		INSERT INTO purchase_item (purchase_id, product_id, quantity)
		SELECT $1, product_id, quantity
		FROM basket
		WHERE shopper_id=$2
	`, purchaseID, shopperID)
	if err != nil {
		return "", err
	}

	// Clear basket
	_, err = tx.Exec(ctx, `DELETE FROM basket WHERE shopper_id=$1`, shopperID)
	if err != nil {
		return "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", err
	}

	b, _ := json.Marshal(map[string]any{"ok": true, "purchase_id": purchaseID, "total": total, "status": "pending"})
	return string(b), nil
}

func (s *Server) toolListPurchases(ctx context.Context, shopperID string, limit int) (string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, total, status, ts
		FROM purchase
		WHERE shopper_id=$1
		ORDER BY ts DESC
		LIMIT $2
	`, shopperID, limit)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type P struct {
		ID     string    `json:"id"`
		Total  string    `json:"total"`
		Status string    `json:"status"`
		TS     time.Time `json:"ts"`
	}
	var ps []P
	for rows.Next() {
		var p P
		if err := rows.Scan(&p.ID, &p.Total, &p.Status, &p.TS); err != nil {
			return "", err
		}
		ps = append(ps, p)
	}

	b, _ := json.Marshal(map[string]any{"ok": true, "purchases": ps})
	return string(b), nil
}

func (s *Server) toolGetPurchaseStatus(ctx context.Context, shopperID, purchaseID string) (string, error) {
	purchaseID = strings.TrimSpace(purchaseID)
	if purchaseID == "" {
		return `{"ok":false,"error":"purchase_id is required"}`, nil
	}

	var status string
	var total string
	var ts time.Time
	err := s.db.QueryRow(ctx, `
		SELECT status, total, ts
		FROM purchase
		WHERE id=$1 AND shopper_id=$2
	`, purchaseID, shopperID).Scan(&status, &total, &ts)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return `{"ok":false,"error":"purchase not found for this shopper"}`, nil
		}
		return "", err
	}

	b, _ := json.Marshal(map[string]any{"ok": true, "purchase_id": purchaseID, "status": status, "total": total, "ts": ts})
	return string(b), nil
}
