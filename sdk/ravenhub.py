"""Cliente Python de RavenHub.

Dos funciones de consumo (`notify` y `status`), un modo best-effort
(`notify_safe`) y un helper de idempotencia por periodo.

    pip install ./sdk            # o copia este archivo (sin dependencias: usa
                                 # requests si está, si no urllib de stdlib)
    export NOTIFY_TOKEN=rvn_proyecto1_...
    export RAVENHUB_URL=https://notify.tudominio.com

    from ravenhub import notify, notify_safe, idempotency_key_for

    notify("Deploy OK", "prod actualizado", to=["oncall"])
    notify("Error crítico", "pipeline caído", to=["oncall"], priority="urgent")

    # Agente que no puede romperse (cron, ciclo autónomo): best-effort.
    notify_safe("Reflexión degradada", "...", to=["oncall"],
                idempotency_key=idempotency_key_for("reflect", period="day"))

Contrato de errores: `notify()`/`status()` lanzan `NotifyError` ante CUALQUIER
fallo (HTTP no-2xx, timeout, red, redirect) — nunca dejan escapar excepciones
de transporte. `notify_safe()` no lanza jamás: devuelve None y deja un warning
en el logger `ravenhub`.

Seguridad: el token viaja en el header Authorization; los redirects NO se
siguen (un 3xx podría llevar el Bearer a otro host) — usa la URL https://
directa.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import urllib.error
import urllib.request
import uuid
from datetime import datetime, timezone
from urllib.parse import urlparse

log = logging.getLogger("ravenhub")

try:  # opcional: sin requests el cliente funciona igual con urllib
    import requests
except ImportError:  # pragma: no cover - depende del entorno
    requests = None

DEFAULT_URL = "http://localhost:8085"
# sync=true espera hasta SyncWait del Hub (default 8s): timeout mayor para no
# cortar la espera en el cliente (5s < 8s siempre fallaba).
TIMEOUT = 15


class NotifyError(RuntimeError):
    """El Hub rechazó la notificación o no se pudo contactar con él."""


def configured(url: str | None = None, token: str | None = None) -> bool:
    """True si hay token configurado (el gate explícito del best-effort)."""
    _, tok = _config(url, token)
    return bool(tok)


_warned_insecure = False


def _config(url: str | None = None, token: str | None = None) -> tuple[str, str]:
    """Resuelve URL y token en CADA llamada: cambiar el entorno después de
    importar el módulo debe surtir efecto (antes la URL se congelaba al import)."""
    global _warned_insecure
    hub = (url or os.getenv("RAVENHUB_URL") or DEFAULT_URL).rstrip("/")
    tok = token if token is not None else (os.getenv("NOTIFY_TOKEN") or "")
    parsed = urlparse(hub)
    if (
        parsed.scheme == "http"
        and parsed.hostname not in ("localhost", "127.0.0.1", "::1")
        and not _warned_insecure
    ):
        # El Bearer y el payload viajan en claro; no rompemos a quien usa un
        # relay local, pero un host remoto por HTTP es una mala configuración.
        log.warning("ravenhub: RAVENHUB_URL usa http:// fuera de loopback; usa https:// para no exponer el token")
        _warned_insecure = True
    return hub, tok


def idempotency_key_for(*parts: object, period: str | None = None) -> str:
    """Clave de idempotencia determinista para "una notificación por periodo".

    Misma entrada + mismo periodo → misma clave → el Hub entrega UNA vez
    (mientras viva el mensaje: ~30 días con la retención por defecto).
    `period`: "hour" | "day" | "week" | "month" (UTC) o None (sin bucket).
    """
    if period is not None:
        parts = tuple(parts) + (_period_bucket(period),)
    # json.dumps: encoding INEQUÍVOCO de los parts; el join con \x1f
    # permitía que ("a\x1fb",) y ("a", "b") produjeran la MISMA clave.
    # sha256 hexdigest son 64 chars fijos; no hace falta truncar.
    raw = json.dumps([str(p) for p in parts], ensure_ascii=False, separators=(",", ":"))
    return "rvnk_" + hashlib.sha256(raw.encode("utf-8")).hexdigest()


def _period_bucket(period: str) -> str:
    now = datetime.now(timezone.utc)
    if period == "hour":
        return now.strftime("%Y-%m-%dT%H")
    if period == "day":
        return now.strftime("%Y-%m-%d")
    if period == "week":
        year, week, _ = now.isocalendar()
        return f"{year}-W{week:02d}"
    if period == "month":
        return now.strftime("%Y-%m")
    raise ValueError(f"period inválido {period!r} (hour|day|week|month)")


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Bloquea redirects en el transporte urllib (fail-closed: el Bearer no
    debe aterrizar en un host distinto al configurado)."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: ANN001
        return None  # urllib lo convierte en HTTPError con el 3xx


def _default_transport(url: str, method: str, payload: dict | None, headers: dict, timeout: float):
    """Transporte real: requests si está instalado, si no urllib (stdlib).
    Devuelve (status_code, headers, text)."""
    body = json.dumps(payload).encode("utf-8") if payload is not None else None
    hdrs = dict(headers)
    if body is not None:
        hdrs.setdefault("Content-Type", "application/json")
    if requests is not None:
        resp = requests.request(
            method, url, data=body, headers=hdrs, timeout=timeout, allow_redirects=False
        )
        return resp.status_code, resp.headers, resp.text
    req = urllib.request.Request(url, data=body, headers=hdrs, method=method)
    opener = urllib.request.build_opener(_NoRedirect())
    try:
        with opener.open(req, timeout=timeout) as resp:
            return resp.status, resp.headers, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:  # incluye 3xx bloqueados por _NoRedirect
        return exc.code, exc.headers, exc.read().decode("utf-8", "replace")


def _request(
    method: str,
    path: str,
    payload: dict | None,
    token: str | None,
    url: str | None,
    timeout: float,
    transport,
    extra_headers: dict | None = None,
) -> dict:
    hub, tok = _config(url, token)
    if not tok:
        raise NotifyError(
            "NOTIFY_TOKEN no definido en el entorno "
            "(export NOTIFY_TOKEN=rvn_... ; para best-effort usa notify_safe)"
        )
    headers = {"Authorization": f"Bearer {tok}"}
    if extra_headers:
        headers.update(extra_headers)
    try:
        code, rheaders, text = (transport or _default_transport)(
            f"{hub}{path}", method, payload, headers, timeout
        )
    except Exception as exc:  # noqa: BLE001 — el contrato ES no propagar
        # OSError solo cubría el path requests (RequestException hereda de
        # OSError). El path urllib —el fallback sin requests— escapaba crudos
        # http.client.IncompleteRead (HTTPException), ValueError de putheader
        # con cabeceras ilegales y TypeError de json.dumps: rompían el ciclo
        # del consumidor pese al contrato. BaseException (Ctrl+C, SystemExit)
        # sigue atravesando deliberadamente.
        raise NotifyError(f"no se pudo contactar el Hub en {hub}: {exc}") from exc
    if 300 <= code < 400:
        raise NotifyError(
            f"el Hub respondió {code}: redirección no permitida "
            "(usa la URL https:// directa; el token no debe saltar de host)"
        )
    if code in (200, 202):
        try:
            return json.loads(text) if text else {}
        except ValueError as exc:
            raise NotifyError(f"respuesta no-JSON del Hub ({code}): {text[:200]!r}") from exc
    if code == 429:
        retry = (rheaders or {}).get("Retry-After", "?")
        raise NotifyError(f"rate limit (reintenta en {retry}s): {text[:300]}")
    raise NotifyError(f"HTTP {code}: {text[:300]}")


def notify(
    title: str,
    message: str,
    to: list[str] | None = None,
    priority: str = "normal",
    idempotency_key: str | None = None,
    template: str | None = None,
    data: dict | None = None,
    tags: list[str] | None = None,
    sync: bool = False,
    url: str | None = None,
    token: str | None = None,
    timeout: float = TIMEOUT,
    transport=None,
    **extra,
) -> dict:
    """Envía una notificación. Devuelve el JSON del Hub ({"id": ...}).

    - `to`: destinos o grupos nombrados del proyecto (los mismos del seed).
    - `idempotency_key`: misma clave → mismo id, un solo envío (dura la vida
      del mensaje en la DB). Sin clave se genera una UUID aleatoria: un
      reintento del caller DUPLICA — para "una por evento" pasa una clave
      estable; para "una por periodo" usa `idempotency_key_for`.
    - `sync=True`: espera el resultado real de entrega (para tests).
    - `transport`: callable(url, method, payload, headers, timeout) →
      (status, headers, text) para tests herméticos.
    - Lanza `NotifyError` ante cualquier fallo (HTTP, red, timeout, redirect).
    """
    _, tok = _config(url, token)
    if not tok:
        raise NotifyError(
            "NOTIFY_TOKEN no definido en el entorno "
            "(export NOTIFY_TOKEN=rvn_... ; para best-effort usa notify_safe)"
        )
    if not to:
        # fail-fast local: el hub rechaza sin 'to' con 400 incondicional —
        # mejor el error claro ANTES de gastar la red (notify_safe lo traga
        # en el borde con su warning, como cualquier otro fallo)
        raise NotifyError("'to' es obligatorio (destinos o grupos del proyecto)")
    payload: dict = {"title": title, "message": message, "priority": priority}
    if to:
        payload["to"] = to
    if template:
        payload["template"] = template
    if data:
        payload["data"] = data
    if tags:
        payload["tags"] = tags
    payload.update(extra)

    key = idempotency_key or str(uuid.uuid4())
    return _request(
        "POST",
        "/v1/notify" + ("?sync=true" if sync else ""),
        payload,
        tok,
        url,
        timeout,
        transport,
        {"Idempotency-Key": key},
    )


def notify_safe(*args, **kwargs) -> dict | None:
    """Best-effort: NUNCA lanza y nunca rompe el ciclo del consumidor.

    - Sin token configurado: no-op con warning (el "desactivado silencioso"
      del consumidor, pero con rastro en el log).
    - Cualquier fallo (HTTP, red, timeout): warning y None.

    Pensado para crons y ciclos autónomos donde "no notificar" es aceptable
    pero "romper el ciclo" no. Para diagnóstico, usa `notify()`.
    """
    _, tok = _config(kwargs.get("url"), kwargs.get("token"))
    if not tok:
        log.warning("ravenhub: NOTIFY_TOKEN sin configurar — notificación omitida (best-effort)")
        return None
    try:
        return notify(*args, **kwargs)
    except Exception as exc:  # noqa: BLE001 — el contrato ES no propagar jamás
        log.warning("ravenhub: notificación no entregada al Hub: %s", exc)
        return None


def status(
    message_id: str,
    url: str | None = None,
    token: str | None = None,
    timeout: float = TIMEOUT,
    transport=None,
) -> dict:
    """Consulta el estado de un mensaje y sus entregas por destino."""
    return _request("GET", f"/v1/messages/{message_id}", None, token, url, timeout, transport)
