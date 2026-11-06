"""Tests del SDK de RavenHub — sin red, con transporte inyectable.

    python3 -m unittest discover -s sdk -p 'test_*.py'   (make test-sdk)
"""

import json
import os
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest import mock

import ravenhub

from ravenhub import (
    NotifyError,
    configured,
    idempotency_key_for,
    notify,
    notify_safe,
    status,
)

ENV = {"NOTIFY_TOKEN": "rvn_test_abc", "RAVENHUB_URL": "http://hub.test"}


def fake_transport(code=202, body='{"id":"m-1"}', headers=None, error=None, calls=None):
    """Transporte falso: registra la llamada y devuelve/levanta lo pedido."""

    def transport(url, method, payload, hdrs, timeout):
        if calls is not None:
            calls.append(
                {"url": url, "method": method, "payload": payload, "headers": hdrs, "timeout": timeout}
            )
        if error is not None:
            raise error
        return code, dict(headers or {}), body

    return transport


class NotifyTests(unittest.TestCase):
    def test_notify_ok_envia_lo_esperado(self):
        calls = []
        with mock.patch.dict(os.environ, ENV, clear=True):
            out = notify(
                "Deploy OK",
                "prod actualizado",
                to=["oncall"],
                priority="high",
                idempotency_key="k-1",
                tags=["deploy"],
                transport=fake_transport(calls=calls),
            )
        self.assertEqual(out, {"id": "m-1"})
        c = calls[0]
        self.assertEqual(c["url"], "http://hub.test/v1/notify")
        self.assertEqual(c["method"], "POST")
        self.assertEqual(c["payload"]["to"], ["oncall"])
        self.assertEqual(c["payload"]["priority"], "high")
        self.assertEqual(c["payload"]["tags"], ["deploy"])
        self.assertEqual(c["headers"]["Authorization"], "Bearer rvn_test_abc")
        self.assertEqual(c["headers"]["Idempotency-Key"], "k-1")

    def test_notify_sync_usa_query(self):
        calls = []
        with mock.patch.dict(os.environ, ENV, clear=True):
            notify("t", "m", to=["d"], sync=True, transport=fake_transport(calls=calls))
        self.assertEqual(calls[0]["url"], "http://hub.test/v1/notify?sync=true")

    def test_sin_token_lanza_y_gate_dice_no(self):
        with mock.patch.dict(os.environ, {"RAVENHUB_URL": "http://hub.test"}, clear=True):
            self.assertFalse(configured())
            with self.assertRaises(NotifyError):
                notify("t", "m", to=["d"], transport=fake_transport())

    def test_url_override(self):
        calls = []
        with mock.patch.dict(os.environ, ENV, clear=True):
            notify("t", "m", to=["d"], url="http://otro:1/", transport=fake_transport(calls=calls))
        self.assertEqual(calls[0]["url"], "http://otro:1/v1/notify")

    def test_env_leido_en_cada_llamada(self):
        calls = []
        with mock.patch.dict(os.environ, ENV, clear=True):
            notify("t", "m", to=["d"], transport=fake_transport(calls=calls))
            os.environ["RAVENHUB_URL"] = "http://hub2.test"
            notify("t", "m", to=["d"], transport=fake_transport(calls=calls))
        self.assertEqual(calls[0]["url"], "http://hub.test/v1/notify")
        self.assertEqual(calls[1]["url"], "http://hub2.test/v1/notify")


class ErrorContractTests(unittest.TestCase):
    def test_error_de_red_envuelto(self):
        with mock.patch.dict(os.environ, ENV, clear=True):
            with self.assertRaises(NotifyError) as ctx:
                notify("t", "m", to=["d"], transport=fake_transport(error=OSError("connection refused")))
        self.assertIn("no se pudo contactar", str(ctx.exception))

    def test_timeout_envuelto(self):
        with mock.patch.dict(os.environ, ENV, clear=True):
            with self.assertRaises(NotifyError):
                notify("t", "m", to=["d"], transport=fake_transport(error=TimeoutError("timed out")))

    def test_redirect_bloqueado(self):
        with mock.patch.dict(os.environ, ENV, clear=True):
            with self.assertRaises(NotifyError) as ctx:
                notify("t", "m", to=["d"], transport=fake_transport(code=301, body=""))
        self.assertIn("redirección", str(ctx.exception))

    def test_429_con_retry_after(self):
        with mock.patch.dict(os.environ, ENV, clear=True):
            with self.assertRaises(NotifyError) as ctx:
                notify("t", "m", to=["d"], transport=fake_transport(code=429, headers={"Retry-After": "7"}, body="slow down"))
        self.assertIn("7s", str(ctx.exception))

    def test_500(self):
        with mock.patch.dict(os.environ, ENV, clear=True):
            with self.assertRaises(NotifyError) as ctx:
                notify("t", "m", to=["d"], transport=fake_transport(code=500, body="boom"))
        self.assertIn("HTTP 500", str(ctx.exception))

    def test_respuesta_no_json(self):
        with mock.patch.dict(os.environ, ENV, clear=True):
            with self.assertRaises(NotifyError):
                notify("t", "m", to=["d"], transport=fake_transport(body="<html>"))

    def test_excepciones_no_oserror_envueltas(self):
        # el path urllib escapa IncompleteRead (HTTPException), ValueError
        # de putheader y TypeError de json.dumps — el contrato es que TODO
        # fallo del transporte llega como NotifyError.
        for exc in (ValueError("cabecera ilegal"), TypeError("no serializable"),
                    RuntimeError("IncompleteRead simulada")):
            with self.subTest(exc=type(exc).__name__):
                with mock.patch.dict(os.environ, ENV, clear=True):
                    with self.assertRaises(NotifyError) as ctx:
                        notify("t", "m", to=["d"], transport=fake_transport(error=exc))
                self.assertIn("no se pudo contactar", str(ctx.exception))

    def test_notify_sin_to_falla_local(self):
        # el hub exige 'to' (400 incondicional): fallar ANTES de la red es el
        # error claro, no el 400 remoto (que notify_safe tragaría mudo)
        calls = []
        with mock.patch.dict(os.environ, ENV, clear=True):
            with self.assertRaises(NotifyError) as ctx:
                notify("t", "m", to=None, transport=fake_transport(calls=calls))
        self.assertIn("'to' es obligatorio", str(ctx.exception))
        self.assertEqual(calls, [])  # no llegó a gastar transporte


class SafeModeTests(unittest.TestCase):
    def test_safe_sin_config_no_rompe_y_avisa(self):
        with mock.patch.dict(os.environ, {"RAVENHUB_URL": "http://hub.test"}, clear=True):
            with self.assertLogs("ravenhub", level="WARNING") as logs:
                self.assertIsNone(notify_safe("t", "m", transport=fake_transport()))
        self.assertIn("sin configurar", "".join(logs.output))

    def test_safe_absorbe_fallos(self):
        with mock.patch.dict(os.environ, ENV, clear=True):
            with self.assertLogs("ravenhub", level="WARNING") as logs:
                out = notify_safe("t", "m", to=["d"], transport=fake_transport(error=OSError("down")))
        self.assertIsNone(out)
        self.assertIn("no entregada", "".join(logs.output))

    def test_safe_ok_devuelve_json(self):
        with mock.patch.dict(os.environ, ENV, clear=True):
            out = notify_safe("t", "m", to=["d"], transport=fake_transport())
        self.assertEqual(out, {"id": "m-1"})


class IdempotencyKeyTests(unittest.TestCase):
    def test_determinista_y_por_periodo(self):
        a = idempotency_key_for("reflect", "svc-a", period="day")
        b = idempotency_key_for("reflect", "svc-a", period="day")
        c = idempotency_key_for("reflect", "svc-b", period="day")
        self.assertEqual(a, b)
        self.assertNotEqual(a, c)
        self.assertLessEqual(len(a), 128)
        self.assertTrue(a.startswith("rvnk_"))
        # sin periodo no colisiona con el bucket del día
        self.assertNotEqual(a, idempotency_key_for("reflect", "svc-a"))

    def test_periodo_invalido(self):
        with self.assertRaises(ValueError):
            idempotency_key_for("x", period="semana")

    def test_parts_ambiguos_no_colisionan(self):
        # el join con \x1f hacía colisionar ("a\x1fb") con ("a", "b")
        self.assertNotEqual(idempotency_key_for("a\x1fb"), idempotency_key_for("a", "b"))
        self.assertNotEqual(
            idempotency_key_for("a", "b\x1fc"), idempotency_key_for("a\x1fb", "c")
        )
        # sigue determinista y acotada
        self.assertEqual(idempotency_key_for("a", "b"), idempotency_key_for("a", "b"))
        self.assertLessEqual(len(idempotency_key_for("x", period="day")), 128)


class StatusTests(unittest.TestCase):
    def test_status_ok(self):
        calls = []
        with mock.patch.dict(os.environ, ENV, clear=True):
            out = status("m-9", transport=fake_transport(body=json.dumps({"id": "m-9", "status": "sent"}), calls=calls))
        self.assertEqual(out["status"], "sent")
        self.assertEqual(calls[0]["method"], "GET")
        self.assertEqual(calls[0]["url"], "http://hub.test/v1/messages/m-9")

    def test_status_envuelve_http(self):
        with mock.patch.dict(os.environ, ENV, clear=True):
            with self.assertRaises(NotifyError):
                status("m-9", transport=fake_transport(code=404, body="not found"))

    def test_status_sin_token(self):
        with mock.patch.dict(os.environ, {"RAVENHUB_URL": "http://hub.test"}, clear=True):
            with self.assertRaises(NotifyError):
                status("m-9", transport=fake_transport())


class _LoopbackHandler(BaseHTTPRequestHandler):
    """Servidor loopback para ejercitar el transporte REAL (path urllib):
    responde 301 si el path empieza por /redirect y 202 JSON al resto."""

    def do_POST(self):
        if self.path.startswith("/redirect"):
            self.send_response(301)
            self.send_header("Location", "/v1/notify")
            self.end_headers()
            return
        n = int(self.headers.get("Content-Length", 0))
        self.rfile.read(n)
        self.send_response(202)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"id": "loop-1"}')

    def log_message(self, *args):
        pass


class DefaultTransportTests(unittest.TestCase):
    """Ejercita _default_transport SIN requests (el fallback urllib) contra
    un servidor en 127.0.0.1 — sin red externa, pero con el path real que
    los agentes usan en hosts sin requests instalado."""

    def setUp(self):
        import ravenhub

        self.rh = ravenhub
        self.saved_requests = ravenhub.requests
        ravenhub.requests = None  # fuerza el path urllib
        self.server = HTTPServer(("127.0.0.1", 0), _LoopbackHandler)
        self.port = self.server.server_address[1]
        self.url = f"http://127.0.0.1:{self.port}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def tearDown(self):
        self.rh.requests = self.saved_requests
        self.server.shutdown()
        self.server.server_close()

    def test_urllib_entrega_202_y_json(self):
        with mock.patch.dict(os.environ, ENV, clear=True):
            out = notify("t", "m", to=["d"], url=self.url)
        self.assertEqual(out, {"id": "loop-1"})

    def test_urllib_bloquea_redirect(self):
        # el Bearer no puede aterrizar en otro host: _NoRedirect convierte el
        # 301 en error y el contrato lo reporta como NotifyError de redirección
        with mock.patch.dict(os.environ, ENV, clear=True):
            with self.assertRaises(NotifyError) as ctx:
                notify("t", "m", to=["d"], url=self.url + "/redirect")
        self.assertIn("redirección", str(ctx.exception))


@unittest.skipUnless(
    getattr(ravenhub, "requests", None) is not None,
    "requests no instalado: el transporte default de producción no es ejercitable",
)
class RequestsTransportTests(unittest.TestCase):
    """El transporte REAL con requests (el default cuando está instalado)
    contra el mismo loopback: 202 con JSON y redirect bloqueado de verdad
    (allow_redirects=False)."""

    def setUp(self):
        self.server = HTTPServer(("127.0.0.1", 0), _LoopbackHandler)
        self.port = self.server.server_address[1]
        self.url = f"http://127.0.0.1:{self.port}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def test_requests_entrega_202_y_json(self):
        with mock.patch.dict(os.environ, ENV, clear=True):
            out = notify("t", "m", to=["d"], url=self.url)
        self.assertEqual(out, {"id": "loop-1"})

    def test_requests_bloquea_redirect(self):
        with mock.patch.dict(os.environ, ENV, clear=True):
            with self.assertRaises(NotifyError) as ctx:
                notify("t", "m", to=["d"], url=self.url + "/redirect")
        self.assertIn("redirección", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()


class InsecureURLWarningTests(unittest.TestCase):
    def setUp(self):
        ravenhub._warned_insecure = False

    def test_http_remoto_avisa(self):
        with mock.patch.dict(
            os.environ,
            {"NOTIFY_TOKEN": "t", "RAVENHUB_URL": "http://example.com"},
            clear=True,
        ):
            with self.assertLogs("ravenhub", level="WARNING") as cm:
                ravenhub.configured()
            self.assertIn("https", "\n".join(cm.output))

    def test_loopback_no_avisa(self):
        with mock.patch.dict(
            os.environ,
            {"NOTIFY_TOKEN": "t", "RAVENHUB_URL": "http://localhost:8085"},
            clear=True,
        ):
            with self.assertNoLogs("ravenhub", level="WARNING"):
                ravenhub.configured()
