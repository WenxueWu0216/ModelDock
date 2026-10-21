import subprocess
import sys
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

ROOT = Path(__file__).resolve().parents[1]


@pytest.fixture(scope="session")
def model_path(tmp_path_factory) -> str:
    out = tmp_path_factory.mktemp("model") / "model.joblib"
    subprocess.run([sys.executable, str(ROOT / "train.py"), "--out", str(out)], check=True)
    return str(out)


@pytest.fixture()
def client(model_path, monkeypatch):
    monkeypatch.setenv("MODEL_PATH", model_path)
    monkeypatch.setenv("MODEL_VERSION", "1.0.0-test")
    sys.path.insert(0, str(ROOT))
    import app.main as main

    with TestClient(main.app) as c:
        yield c


def test_health(client):
    r = client.get("/health")
    assert r.status_code == 200
    assert r.json() == {"status": "ok", "model_version": "1.0.0-test"}


def test_predict(client):
    r = client.post("/predict", json={"instances": [[5.1, 3.5, 1.4, 0.2], [6.7, 3.0, 5.2, 2.3]]})
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["predictions"] == ["setosa", "virginica"]
    assert all(abs(sum(p) - 1) < 1e-4 for p in body["probabilities"])
    assert body["model_version"] == "1.0.0-test"


@pytest.mark.parametrize(
    "payload",
    [{"instances": []}, {"instances": [[1, 2, 3]]}, {"instances": "nope"}, {}],
)
def test_predict_rejects_bad_input(client, payload):
    assert client.post("/predict", json=payload).status_code == 422


def test_metrics(client):
    client.post("/predict", json={"instances": [[5.1, 3.5, 1.4, 0.2]]})
    text = client.get("/metrics").text
    assert 'model_predictions_total{status="ok"}' in text
    assert "model_prediction_latency_seconds_bucket" in text
    assert 'model_info{accuracy="' in text


def test_model_accuracy(model_path):
    import joblib

    assert joblib.load(model_path)["accuracy"] >= 0.9
