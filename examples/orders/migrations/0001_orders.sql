CREATE TABLE IF NOT EXISTS orders (
  id          uuid PRIMARY KEY,
  customer_id uuid        NOT NULL,
  status      text        NOT NULL CHECK (status IN ('draft', 'placed')),
  lines       jsonb       NOT NULL DEFAULT '[]'::jsonb,
  version     integer     NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS orders_customer_idx ON orders (customer_id);
