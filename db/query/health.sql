-- name: HealthCheck :one
SELECT CASE
    WHEN to_regclass('public.users') IS NOT NULL
    AND to_regclass('public.accounts') IS NOT NULL
    AND to_regclass('public.auth_sessions') IS NOT NULL
    AND to_regclass('public.refresh_tokens') IS NOT NULL
    THEN 1::integer
    ELSE 0::integer
END;

-- name: OrderHealthCheck :one
SELECT CASE
    WHEN to_regclass('public.users') IS NOT NULL
    AND to_regclass('public.accounts') IS NOT NULL
    AND to_regclass('public.products') IS NOT NULL
    AND to_regclass('public.inventories') IS NOT NULL
    AND to_regclass('public.orders') IS NOT NULL
    AND to_regclass('public.order_items') IS NOT NULL
    THEN 1::integer
    ELSE 0::integer
END;
