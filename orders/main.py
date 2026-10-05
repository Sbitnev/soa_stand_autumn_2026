"""Orders: оформление заказа, устойчивое к отказам Payment.

Идея в трёх пунктах:
1. Заказ сначала записывается в Postgres как `pending` и только потом идёт в
   Payment. Если процесс упадёт, заказ не потеряется: его подберёт воркер.
2. Каждый запрос в Payment идёт с `Idempotency-Key = id заказа`. Поэтому
   повторять его можно сколько угодно: деньги второй раз не спишутся.
3. POST /orders ждёт не дольше FAST_BUDGET. Не успели -- отвечаем `pending`,
   а доделывает фоновый воркер. Если Payment лежит, circuit breaker не даёт
   слать ему запросы на каждый заказ.
"""
import asyncio
import json
import logging
import os
import random
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

FAST_BUDGET = 0.7      # сек на всю быструю попытку в POST /orders (лимит раннера -- 1 с)
REQUEST_TIMEOUT = 5.0  # сек на один запрос в фоне: должно хватать на медленный, но живой Payment
LEASE = 15             # сек: на столько воркер "занимает" заказ, пока обрабатывает его
POLL_INTERVAL = 0.5    # как часто воркер заглядывает в БД
BATCH = 20             # сколько заказов воркер берёт за раз

SCHEMA = """
CREATE TABLE IF NOT EXISTS orders (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL,
    status          TEXT NOT NULL,                  -- paid | rejected | pending
    amount_cents    BIGINT NOT NULL,
    items           JSONB NOT NULL,
    declined        BOOLEAN NOT NULL DEFAULT false, -- банк сказал 402, резерв ещё не снят
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS orders_pending_idx ON orders (next_attempt_at) WHERE status = 'pending';
"""

pool: asyncpg.Pool
http: httpx.AsyncClient


# ---------------------------------------------------------------- circuit breaker

class Breaker:
    """Защита Payment: после FAIL_LIMIT неудач подряд перестаём слать запросы.

    "Неудача" здесь -- только когда Payment НЕ ОТВЕЧАЕТ: таймаут или нет
    соединения. Явный ответ (даже 500) или обрыв соединения после приёма
    запроса значит, что Payment жив: такой заказ просто повторится позже
    с паузой (retry_later), а общий выключатель не нужен.

    Пока "открыт" -- запросы не идут вообще. Когда пауза прошла, пропускаем
    ровно один пробный запрос. Успех -- закрываемся, неудача -- открываемся
    снова на вдвое более долгую паузу (до MAX_COOLDOWN).
    """
    FAIL_LIMIT = 3
    BASE_COOLDOWN = 5.0
    MAX_COOLDOWN = 10.0

    def __init__(self):
        self.failures = 0
        self.open_until = 0.0
        self.cooldown = self.BASE_COOLDOWN
        self.probing = False

    def allow(self) -> bool:
        if time.monotonic() < self.open_until:
            return False
        if self.failures >= self.FAIL_LIMIT:  # пауза прошла: один пробный запрос
            if self.probing:
                return False
            self.probing = True
        return True

    def success(self):
        self.failures = 0
        self.cooldown = self.BASE_COOLDOWN
        self.open_until = 0.0
        self.probing = False

    def failure(self):
        self.failures += 1
        self.probing = False
        if self.failures >= self.FAIL_LIMIT:
            self.open_until = time.monotonic() + self.cooldown
            self.cooldown = min(self.cooldown * 2, self.MAX_COOLDOWN)
            log.warning("breaker открыт на %.0f с", self.open_until - time.monotonic())


breaker = Breaker()


# ---------------------------------------------------------------- вызовы внешних сервисов

async def reserve(o: dict, timeout: float) -> bool:
    """Резерв в Inventory. Повторный вызов безопасен (вернёт 200, а не новый резерв)."""
    items = [{"sku": i["sku"], "qty": i["qty"]} for i in o["items"]]
    try:
        r = await http.post(f"{INVENTORY_URL}/reservations", timeout=timeout,
                            json={"order_id": o["id"], "items": items})
    except httpx.HTTPError as e:
        log.warning("резерв %s: %r", o["id"], e)
        return False
    return r.status_code in (200, 201)


async def release(o: dict, timeout: float) -> bool:
    """Снять резерв. Повтор безопасен (Inventory всегда отвечает 204)."""
    try:
        r = await http.delete(f"{INVENTORY_URL}/reservations/{o['id']}", timeout=timeout)
    except httpx.HTTPError as e:
        log.warning("снятие резерва %s: %r", o["id"], e)
        return False
    return r.status_code in (200, 204)


async def charge(o: dict, timeout: float) -> str:
    """Списание в Payment. Возвращает:
    paid     -- деньги списаны (201) или уже были списаны ранее с этим ключом (200);
    declined -- банк отказал (402), деньги не списаны, повторять не надо;
    failed   -- 500 / таймаут / обрыв: деньги не списаны ИЛИ неизвестно, повторим с тем же ключом;
    skip     -- не стали обращаться (breaker открыт или не осталось времени).
    """
    if timeout < 0.05 or not breaker.allow():
        return "skip"
    try:
        r = await http.post(
            f"{PAYMENT_URL}/payments", timeout=timeout,
            # Ключ = id заказа: повтор вернёт тот же результат, а не новое списание.
            headers={"Idempotency-Key": o["id"]},
            json={"order_id": o["id"], "amount_cents": o["amount_cents"], "currency": "RUB"},
        )
    except (httpx.TimeoutException, httpx.ConnectError) as e:
        # Payment не отвечает вовремя / недоступен. Деньги могли списаться --
        # узнаем повтором с тем же ключом. Это сигнал для breaker.
        log.warning("платёж %s: %r", o["id"], e)
        breaker.failure()
        return "failed"
    except httpx.HTTPError as e:
        # Соединение оборвано без ответа (потерянный ответ): Payment жив, но
        # результат неизвестен. Повторим с тем же ключом; breaker не трогаем.
        log.warning("платёж %s: %r", o["id"], e)
        breaker.success()
        return "failed"
    except BaseException:
        breaker.probing = False  # отмена задачи: не оставляем breaker с "висящей" пробой
        raise
    if r.status_code in (200, 201):
        breaker.success()
        return "paid"
    if r.status_code == 402:
        breaker.success()  # Payment жив и отвечает
        return "declined"
    # 500 и прочее: Payment ответил, значит жив; деньги не списаны, повторим позже.
    log.warning("платёж %s: код %s", o["id"], r.status_code)
    breaker.success()
    return "failed"


# ---------------------------------------------------------------- обработка заказа

async def set_status(order_id: str, status: str):
    await pool.execute("UPDATE orders SET status = $2 WHERE id = $1", order_id, status)


async def retry_later(order_id: str, failed: bool):
    """Оставляем заказ pending и назначаем время следующей попытки.

    failed=True -- была реальная неудача: увеличиваем счётчик и ждём дольше
    (экспоненциально, с разбросом, потолок 10 с). failed=False -- просто не
    дошли до Payment (breaker): через секунду посмотрим снова.
    """
    if failed:
        n = await pool.fetchval("SELECT attempts FROM orders WHERE id = $1", order_id)
        delay = min(2 ** (n or 0), 10) * (0.5 + random.random())
    else:
        delay = 1.0
    await pool.execute(
        "UPDATE orders SET attempts = attempts + $3, "
        "next_attempt_at = now() + make_interval(secs => $2) "
        "WHERE id = $1 AND status = 'pending'",
        order_id, delay, 1 if failed else 0)


async def process(o: dict, budget: float) -> str:
    """Один проход по заказу. Возвращает статус: paid / rejected / pending.

    Вызывается и из POST /orders (budget маленький), и из воркера (budget большой).
    Безопасно вызывать повторно, на любом шаге: все внешние вызовы идемпотентны,
    а что уже сделано, записано в БД.
    """
    deadline = time.monotonic() + budget

    def left() -> float:
        return min(deadline - time.monotonic(), REQUEST_TIMEOUT)

    try:
        if not o["declined"]:
            # Шаг 1: резерв.
            if not await reserve(o, left()):
                await retry_later(o["id"], failed=True)
                return "pending"
            # Шаг 2: деньги.
            result = await charge(o, left())
            if result == "paid":
                await set_status(o["id"], "paid")
                return "paid"
            if result != "declined":
                await retry_later(o["id"], failed=(result == "failed"))
                return "pending"
            # 402: запоминаем отказ в БД ДО снятия резерва. Если упадём между
            # ними, воркер увидит declined=true и только снимет резерв.
            await pool.execute("UPDATE orders SET declined = true WHERE id = $1", o["id"])
        # Шаг 3 (только после 402): снять резерв. rejected ставим лишь после
        # успешного DELETE -- иначе "rejected" означал бы резерв, висящий на складе.
        if await release(o, left()):
            await set_status(o["id"], "rejected")
            return "rejected"
        await retry_later(o["id"], failed=True)
    except Exception:
        log.exception("process %s", o["id"])
    return "pending"


async def worker():
    """Фон: берёт из БД заказы pending, чьё время пришло, и доводит их до конца."""
    while True:
        try:
            # "Занимаем" заказы: сдвигаем next_attempt_at на LEASE секунд вперёд,
            # чтобы их не взяли повторно, пока мы заняты. Если процесс упадёт --
            # через LEASE секунд заказ снова станет доступен.
            rows = await pool.fetch(
                "UPDATE orders SET next_attempt_at = now() + make_interval(secs => $1) "
                "WHERE id IN (SELECT id FROM orders WHERE status = 'pending' AND next_attempt_at <= now() "
                "             ORDER BY next_attempt_at LIMIT $2 FOR UPDATE SKIP LOCKED) "
                "RETURNING id, amount_cents, items, declined",
                float(LEASE), BATCH)
            orders = [{**dict(r), "items": json.loads(r["items"])} for r in rows]
            if orders:
                await asyncio.gather(*(process(o, LEASE - 3) for o in orders))
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
    http = httpx.AsyncClient()
    task = asyncio.create_task(worker())
    yield
    task.cancel()
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
    # Сначала в БД. next_attempt_at +3 с: пока идёт быстрая попытка ниже,
    # воркер этот заказ не трогает; не успеем -- он подберёт его сам.
    await pool.execute(
        "INSERT INTO orders (id, user_id, status, amount_cents, items, next_attempt_at) "
        "VALUES ($1, $2, 'pending', $3, $4, now() + interval '3 seconds')",
        order_id, req.user_id, amount, json.dumps(items))
    order = {"id": order_id, "amount_cents": amount, "items": items, "declined": False}
    status = await process(order, FAST_BUDGET)
    log.info("order %s: %s", order_id, status)
    return {"id": order_id, "status": status}


@app.get("/orders/{order_id}")
async def get_order(order_id: str):
    row = await pool.fetchrow("SELECT id, status, amount_cents FROM orders WHERE id = $1", order_id)
    if row is None:
        return JSONResponse({"error": "not_found"}, status_code=404)
    return dict(row)


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=int(os.environ.get("PORT", "8080")), log_level="warning")
