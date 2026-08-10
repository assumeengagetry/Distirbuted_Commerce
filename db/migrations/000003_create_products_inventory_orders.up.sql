CREATE TABLE products (
    id UUID PRIMARY KEY,
    sku VARCHAR(64) NOT NULL,
    name VARCHAR(200) NOT NULL,
    description VARCHAR(2000) NOT NULL DEFAULT '',
    price_amount BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL DEFAULT 'USD',
    status VARCHAR(20) NOT NULL DEFAULT 'inactive',
    version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT products_sku_unique UNIQUE (sku),
    CONSTRAINT products_sku_canonical CHECK (
        sku = upper(sku)
        AND sku = btrim(sku)
        AND sku ~ '^[A-Z0-9][A-Z0-9._-]{2,63}$'
    ),
    CONSTRAINT products_name_length CHECK (char_length(btrim(name)) BETWEEN 1 AND 200),
    CONSTRAINT products_description_length CHECK (char_length(description) <= 2000),
    CONSTRAINT products_price_range CHECK (price_amount BETWEEN 1 AND 1000000000000),
    CONSTRAINT products_currency_iso CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT products_status_valid CHECK (status IN ('active', 'inactive')),
    CONSTRAINT products_version_positive CHECK (version > 0),
    CONSTRAINT products_timestamps_valid CHECK (updated_at >= created_at)
);

CREATE TABLE inventories (
    product_id UUID PRIMARY KEY,
    quantity BIGINT NOT NULL DEFAULT 0,
    version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT inventories_product_fk FOREIGN KEY (product_id) REFERENCES products (id) ON DELETE RESTRICT,
    CONSTRAINT inventories_quantity_range CHECK (quantity BETWEEN 0 AND 1000000000),
    CONSTRAINT inventories_version_positive CHECK (version > 0),
    CONSTRAINT inventories_timestamps_valid CHECK (updated_at >= created_at)
);

CREATE TABLE orders (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL,
    status VARCHAR(30) NOT NULL DEFAULT 'pending',
    currency VARCHAR(3) NOT NULL,
    total_amount BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
    CONSTRAINT orders_status_valid CHECK (status IN ('pending', 'paid', 'payment_failed', 'cancelled')),
    CONSTRAINT orders_currency_iso CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT orders_total_positive CHECK (total_amount > 0),
    CONSTRAINT orders_timestamps_valid CHECK (updated_at >= created_at)
);

CREATE TABLE order_items (
    id UUID PRIMARY KEY,
    order_id UUID NOT NULL,
    product_id UUID NOT NULL,
    product_sku VARCHAR(64) NOT NULL,
    product_name VARCHAR(200) NOT NULL,
    product_version BIGINT NOT NULL,
    quantity BIGINT NOT NULL,
    unit_price_amount BIGINT NOT NULL,
    line_amount BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT order_items_order_product_unique UNIQUE (order_id, product_id),
    CONSTRAINT order_items_order_fk FOREIGN KEY (order_id) REFERENCES orders (id) ON DELETE CASCADE,
    CONSTRAINT order_items_product_fk FOREIGN KEY (product_id) REFERENCES products (id) ON DELETE RESTRICT,
    CONSTRAINT order_items_product_version_positive CHECK (product_version > 0),
    CONSTRAINT order_items_quantity_range CHECK (quantity BETWEEN 1 AND 1000),
    CONSTRAINT order_items_unit_price_range CHECK (unit_price_amount BETWEEN 1 AND 1000000000000),
    CONSTRAINT order_items_line_positive CHECK (line_amount > 0),
    CONSTRAINT order_items_line_correct CHECK (
        line_amount::numeric = unit_price_amount::numeric * quantity::numeric
    )
);

CREATE INDEX products_active_created_idx
    ON products (created_at DESC, id DESC)
    WHERE status = 'active';
CREATE INDEX products_created_idx ON products (created_at DESC, id DESC);
CREATE INDEX inventories_updated_idx ON inventories (updated_at DESC, product_id DESC);
CREATE INDEX orders_user_history_idx ON orders (user_id, created_at DESC, id DESC);
CREATE INDEX orders_global_history_idx ON orders (created_at DESC, id DESC);
CREATE INDEX orders_status_idx ON orders (status, created_at DESC);
CREATE INDEX order_items_order_idx ON order_items (order_id, id);
CREATE INDEX order_items_product_idx ON order_items (product_id);
