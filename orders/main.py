"""Orders: оформление заказа, устойчивое к отказам Payment.

Схема простая:
1. POST /orders только записывает заказ в Postgres со статусом `pending` и сразу
   отвечает. Во внешние сервисы он не ходит, поэтому отвечает быстро всегда.
2. Фоновые воркеры берут `pending`-заказы из БД и доводят их до `paid` / `rejected`.
   Не получилось (ошибка, таймаут, обрыв) -- заказ остаётся `pending` и берётся снова.
3. Каждый запрос в Payment идёт с `Idempotency-Key = id заказа`: повторять можно
   сколько угодно, деньги второй раз не спишутся.
"""
import asyncio
import json
import logging
import os
import time
import uuid

import asyncpg
import httpx
import uvicorn
from fastapi import FastAPI
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field, StrictInt

log = logging.getLogger("orders")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

PAYMENT_URL = os.environ["PAYMENT_URL"].rstrip("/")
INVENTORY_URL = os.environ["INVENTORY_URL"].rstrip("/")
DATABASE_URL = os.environ["DATABASE_URL"]

WORKERS = 5            # сколько заказов обрабатывается одновременно
REQUEST_TIMEOUT = 5.0  # сек на один внешний запрос (без таймаута воркер зависнет вместе с Payment)
RETRY_DELAY = 2        # сек до повторной попытки неудавшегося заказа
LEASE = 30             # сек: на столько воркер "занимает" заказ (защита от падения посреди обработки)
PAYMENT_PAUSE = 10.0   # сек не ходим в Payment после таймаута
POLL_INTERVAL = 0.5    # сек: пауза воркера, когда заказов нет

SCHEMA = """
CREATE TABLE IF NOT EXISTS orders (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL,
    status          TEXT NOT NULL,                  -- paid | rejected | pending
    amount_cents    BIGINT NOT NULL,
    items           JSONB NOT NULL,
    declined        BOOLEAN NOT NULL DEFAULT false, -- банк сказал 402, резерв ещё не снят
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
"""

pool: asyncpg.Pool
http: httpx.AsyncClient


# ---------------------------------------------------------------- защита Payment
# Если Payment не отвечает (таймаут), на PAYMENT_PAUSE секунд перестаём туда ходить.
# Когда пауза прошла, пускаем ОДИН пробный запрос: ответил -- работаем дальше,
# снова молчит -- ещё пауза. Так лежащий Payment не получает запрос на каждый заказ.

down_until = None  # None -- Payment отвечает; иначе момент, до которого в него не ходим
probing = False    # идёт ли сейчас пробный запрос


def payment_allowed() -> bool:
    global probing
    if down_until is None:
        return True
    if time.monotonic() < down_until or probing:
        return False
    probing = True  # пауза прошла: пускаем один пробный запрос
    return True


def payment_alive():
    global down_until, probing
    down_until, probing = None, False


def payment_down():
    global down_until, probing
    down_until, probing = time.monotonic() + PAYMENT_PAUSE, False
    log.warning("Payment не отвечает, пауза %.0f с", PAYMENT_PAUSE)


# ---------------------------------------------------------------- вызовы внешних сервисов

async def reserve(o: dict) -> bool:
    """Резерв в Inventory. Повторный вызов безопасен: вернёт 200 с тем же резервом."""
    items = [{"sku": i["sku"], "qty": i["qty"]} for i in o["items"]]
    try:
        r = await http.post(f"{INVENTORY_URL}/reservations", json={"order_id": o["id"], "items": items})
    except httpx.HTTPError as e:
        log.warning("резерв %s: %r", o["id"], e)
        return False
    return r.status_code in (200, 201)


async def release(o: dict) -> bool:
    """Снять резерв. Повторный вызов безопасен: Inventory всегда отвечает 204."""
    try:
        r = await http.delete(f"{INVENTORY_URL}/reservations/{o['id']}")
    except httpx.HTTPError as e:
        log.warning("снятие резерва %s: %r", o["id"], e)
        return False
    return r.status_code in (200, 204)


async def charge(o: dict) -> str:
    """Списание в Payment. Возвращает:
    paid     -- деньги списаны (201) или уже были списаны ранее с этим ключом (200);
    declined -- банк отказал (402), деньги не списаны, повторять не надо;
    retry    -- не знаем исход или деньги не списаны: попробуем позже с тем же ключом.
    """
    if not payment_allowed():
        return "retry"
    try:
        r = await http.post(
            f"{PAYMENT_URL}/payments",
            # Ключ = id заказа: повтор вернёт тот же результат, а не новое списание.
            headers={"Idempotency-Key": o["id"]},
            json={"order_id": o["id"], "amount_cents": o["amount_cents"], "currency": "RUB"},
        )
    except (httpx.TimeoutException, httpx.ConnectError) as e:
        log.warning("платёж %s: %r", o["id"], e)
        payment_down()  # Payment молчит или недоступен
        return "retry"
    except httpx.HTTPError as e:
        # Обрыв без ответа: операция могла выполниться (потерянный ответ), но
        # Payment жив. Повторим с тем же ключом -- вернётся результат первой попытки.
        log.warning("платёж %s: %r", o["id"], e)
        payment_alive()
        return "retry"
    payment_alive()  # получили ответ -- Payment жив
    if r.status_code in (200, 201):
        return "paid"
    if r.status_code == 402:
        return "declined"
    log.warning("платёж %s: код %s", o["id"], r.status_code)  # 500: деньги не списаны
    return "retry"


# ---------------------------------------------------------------- обработка заказа

async def set_status(order_id: str, status: str):
    await pool.execute("UPDATE orders SET status = $2 WHERE id = $1", order_id, status)


async def retry_later(order_id: str):
    """Оставляем заказ pending: воркер возьмёт его снова через RETRY_DELAY секунд."""
    await pool.execute(
        "UPDATE orders SET next_attempt_at = now() + make_interval(secs => $2) WHERE id = $1",
        order_id, float(RETRY_DELAY))


async def process(o: dict):
    """Один проход по заказу. Безопасно повторять на любом шаге: внешние вызовы
    идемпотентны, а что уже сделано -- записано в БД. Если что-то не вышло,
    заказ остаётся pending и будет взят снова."""
    try:
        if not o["declined"]:
            if not await reserve(o):
                return await retry_later(o["id"])
            result = await charge(o)
            if result == "paid":
                return await set_status(o["id"], "paid")
            if result == "retry":
                return await retry_later(o["id"])
            # 402: запоминаем отказ в БД ДО снятия резерва. Если упадём между
            # ними, воркер увидит declined=true и только снимет резерв.
            await pool.execute("UPDATE orders SET declined = true WHERE id = $1", o["id"])
        # rejected ставим лишь после успешного снятия резерва, иначе "rejected"
        # означал бы резерв, который висит на складе.
        if await release(o):
            await set_status(o["id"], "rejected")
        else:
            await retry_later(o["id"])
    except Exception:
        log.exception("process %s", o["id"])  # заказ вернётся сам, когда истечёт LEASE


async def worker():
    """Берёт из БД по одному pending-заказу, чьё время пришло, и обрабатывает."""
    while True:
        try:
            # Одним запросом выбираем заказ и "занимаем" его: сдвигаем next_attempt_at
            # на LEASE секунд вперёд, чтобы другой воркер его не взял. Если процесс
            # упадёт -- через LEASE секунд заказ снова станет доступен.
            # FOR UPDATE SKIP LOCKED: воркеры не ждут друг друга и не берут один заказ.
            row = await pool.fetchrow(
                "UPDATE orders SET next_attempt_at = now() + make_interval(secs => $1) "
                "WHERE id = (SELECT id FROM orders WHERE status = 'pending' AND next_attempt_at <= now() "
                "            ORDER BY next_attempt_at LIMIT 1 FOR UPDATE SKIP LOCKED) "
                "RETURNING id, amount_cents, items, declined",
                float(LEASE))
            if row:
                await process({**dict(row), "items": json.loads(row["items"])})
                continue
        except Exception:
            log.exception("worker")
        await asyncio.sleep(POLL_INTERVAL)


# ---------------------------------------------------------------- HTTP

class Item(BaseModel):
    sku: str = Field(min_length=1)
    qty: StrictInt = Field(ge=1)
    price_cents: StrictInt = Field(ge=0)


class OrderRequest(BaseModel):
    user_id: str = Field(min_length=1)
    items: list[Item] = Field(min_length=1)


async def lifespan(_: FastAPI):
    global pool, http
    for _attempt in range(60):  # Postgres может подняться позже сервиса
        try:
            pool = await asyncpg.create_pool(DATABASE_URL)
            await pool.execute(SCHEMA)
            break
        except (OSError, asyncpg.PostgresError) as e:
            log.info("база недоступна, жду: %r", e)
            await asyncio.sleep(1)
    else:
        raise RuntimeError("база недоступна")
    http = httpx.AsyncClient(timeout=REQUEST_TIMEOUT)
    tasks = [asyncio.create_task(worker()) for _ in range(WORKERS)]
    yield
    for t in tasks:
        t.cancel()
    await http.aclose()
    await pool.close()


app = FastAPI(lifespan=lifespan)


@app.exception_handler(RequestValidationError)
async def bad_request(_, __):
    # FastAPI по умолчанию отвечает 422, а по контракту плохое тело -- 400.
    return JSONResponse({"error": "bad_request"}, status_code=400)


@app.get("/healthz")
async def healthz():
    return {"status": "ok"}


@app.post("/orders", status_code=201)
async def create_order(req: OrderRequest):
    order_id = str(uuid.uuid4())
    items = [i.model_dump() for i in req.items]
    amount = sum(i["qty"] * i["price_cents"] for i in items)
    # Только запись в БД: дальше заказ подхватит воркер.
    await pool.execute(
        "INSERT INTO orders (id, user_id, status, amount_cents, items) VALUES ($1, $2, 'pending', $3, $4)",
        order_id, req.user_id, amount, json.dumps(items))
    return {"id": order_id, "status": "pending"}


@app.get("/orders/{order_id}")
async def get_order(order_id: str):
    row = await pool.fetchrow("SELECT id, status, amount_cents FROM orders WHERE id = $1", order_id)
    if row is None:
        return JSONResponse({"error": "not_found"}, status_code=404)
    return dict(row)


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=int(os.environ.get("PORT", "8080")), log_level="warning")
