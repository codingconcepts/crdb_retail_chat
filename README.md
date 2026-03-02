# crdb_retail_chat
A simple agentic retail assistant using CockroachDB, HTMX, OpenAI, and custom OpenAI tooling.

### Setup

Cluster

```sh
cockroach demo --insecure --no-example-database
```

Objects

```sql
CREATE TABLE IF NOT EXISTS shopper (
  "id" UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  "email" STRING NOT NULL,

  UNIQUE("email")
);

CREATE TABLE IF NOT EXISTS product (
  "id" UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  "name" STRING NOT NULL,
  "price" DECIMAL NOT NULL,

  INDEX ("name")
);

CREATE TABLE IF NOT EXISTS basket (
  "shopper_id" UUID NOT NULL REFERENCES shopper("id"),
  "product_id" UUID NOT NULL REFERENCES product("id"),
  "quantity" INT NOT NULL DEFAULT 1,

  PRIMARY KEY ("shopper_id", "product_id")
);

CREATE TYPE IF NOT EXISTS purchase_status AS ENUM ('pending', 'payment_successful', 'payment_failed', 'dispatched');

CREATE TABLE IF NOT EXISTS purchase (
  "id" UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  "shopper_id" UUID NOT NULL REFERENCES shopper("id"),
  "total" DECIMAL NOT NULL,
  "status" purchase_status NOT NULL DEFAULT 'pending',
  "ts" TIMESTAMPTZ NOT NULL DEFAULT now(),

  INDEX ("shopper_id") STORING ("total", "ts")
);

CREATE TABLE IF NOT EXISTS purchase_item (
  "id" UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  "purchase_id" UUID NOT NULL REFERENCES purchase("id"),
  "product_id" UUID NOT NULL REFERENCES product("id"),
  "quantity" INT NOT NULL
);

CREATE OR REPLACE FUNCTION checkout(shopper_id_in UUID) RETURNS UUID AS $$
DECLARE
  purchase_id UUID;
BEGIN
  -- Create purchase.
  INSERT INTO purchase (shopper_id, total)
    SELECT 
      b.shopper_id,
      SUM(b.quantity * p.price) as total
    FROM basket b
    JOIN product p ON b.product_id = p.id
    WHERE b.shopper_id = shopper_id_in
    GROUP BY b.shopper_id
  RETURNING id INTO purchase_id;

  -- Create purchase lines.
  INSERT INTO purchase_item (purchase_id, product_id, quantity)
    SELECT
      purchase_id, 
      b.product_id,
      b.quantity
    FROM basket b
    WHERE b.shopper_id = shopper_id_in;

  -- Clear basket.
  DELETE FROM basket
  WHERE shopper_id = shopper_id_in;

  RETURN purchase_id;

END;
$$ LANGUAGE PLPGSQL;
```

Data

```sql
INSERT INTO product (name, price) VALUES
  ('Colombian Dark Roast Coffee Beans 1kg', 14.99),
  ('Ethiopian Single Origin Coffee Beans 250g', 9.50),
  ('Organic Green Tea 100 Bags', 6.25),
  ('Ceramic Coffee Mug 350ml', 8.75),
  ('French Press 1L', 24.99),
  ('Electric Burr Coffee Grinder', 89.00),
  ('Stainless Steel Travel Mug 500ml', 19.95),
  ('Chocolate Covered Espresso Beans 200g', 5.99);

INSERT INTO shopper (id, email) VALUES
  ('0ba8f6c1-5817-456f-9503-69e0799faa0b', 'alice@example.com'),
  (gen_random_uuid(), 'bob@example.com'),
  (gen_random_uuid(), 'charlie@example.com'),
  (gen_random_uuid(), 'diana@example.com');
```

Agent

```sh
source .env
go run main.go
```

### Conversation

```sh
open http://localhost:8080
```

> Do you have any Columbian coffee?

> Yes please, 2 bags.

> Do you have anything I could use to drink my coffee with?

> Just one of the ceramic mugs please.

> What's currently in my basket?

> Remove one of the coffee bags from my basket.

> Let's checkout.

> Is there anything in my basket?

> Show my my recent purchases.

### Useful queries

Fetch basket

```sql
SELECT
  p.name,
  b.quantity
FROM basket b
JOIN product p ON b.product_id = p.id
WHERE b.shopper_id = '0ba8f6c1-5817-456f-9503-69e0799faa0b';
```

Fetch purchases

```sql
SELECT
  p.id,
  p.status,
  pr.name,
  pr.price,
  pi.quantity,
  (pr.price * pi.quantity) AS item_total,
  p.total AS purchase_total
FROM purchase p
JOIN purchase_item pi ON pi.purchase_id = p.id
JOIN product pr ON pr.id = pi.product_id
WHERE p.shopper_id = '0ba8f6c1-5817-456f-9503-69e0799faa0b'
ORDER BY p.ts DESC;
```