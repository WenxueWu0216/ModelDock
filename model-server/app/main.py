"""Sample inference server deployed by ModelDock.

Endpoints:
  GET  /health   readiness/liveness (200 once the model is loaded)
  POST /predict  {"instances": [[sepal_len, sepal_wid, petal_len, petal_wid], ...]}
  GET  /metrics  Prometheus metrics
"""

import os
import time
from contextlib import asynccontextmanager
from typing import Any

import joblib
import numpy as np
from fastapi import FastAPI, HTTPException, Response
from prometheus_client import CONTENT_TYPE_LATEST, Counter, Gauge, Histogram, generate_latest
from pydantic import BaseModel, Field, field_validator

MAX_BATCH = int(os.environ.get("MAX_BATCH", "256"))

PREDICTIONS = Counter("model_predictions_total", "Prediction requests.", ["status"])
INSTANCES = Counter("model_predicted_instances_total", "Instances scored.")
LATENCY = Histogram(
    "model_prediction_latency_seconds",
    "Prediction latency.",
    buckets=(0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25),
)
MODEL_INFO = Gauge("model_info", "Loaded model metadata.", ["version", "accuracy"])

state: dict[str, Any] = {}


def load_model(path: str, version: str) -> dict[str, Any]:
    bundle = joblib.load(path)
    MODEL_INFO.labels(version=version, accuracy=f"{bundle['accuracy']:.4f}").set(1)
    return bundle


@asynccontextmanager
async def lifespan(_: FastAPI):
    # Read config at startup (not import) so tests and the container agree.
    state["version"] = os.environ.get("MODEL_VERSION", "dev")
    state["bundle"] = load_model(os.environ.get("MODEL_PATH", "/app/model.joblib"), state["version"])
    yield
    state.clear()


app = FastAPI(title="iris-model-server", lifespan=lifespan)


class PredictRequest(BaseModel):
    instances: list[list[float]] = Field(min_length=1)

    @field_validator("instances")
    @classmethod
    def check_shape(cls, v: list[list[float]]) -> list[list[float]]:
        if len(v) > MAX_BATCH:
            raise ValueError(f"at most {MAX_BATCH} instances per request")
        if any(len(row) != 4 for row in v):
            raise ValueError("each instance must have exactly 4 features")
        return v


class PredictResponse(BaseModel):
    predictions: list[str]
    probabilities: list[list[float]]
    model_version: str


@app.get("/health")
def health() -> dict[str, str]:
    if "bundle" not in state:
        raise HTTPException(status_code=503, detail="model not loaded")
    return {"status": "ok", "model_version": state["version"]}


@app.post("/predict", response_model=PredictResponse)
def predict(req: PredictRequest) -> PredictResponse:
    bundle = state.get("bundle")
    if bundle is None:
        PREDICTIONS.labels(status="unavailable").inc()
        raise HTTPException(status_code=503, detail="model not loaded")
    start = time.perf_counter()
    try:
        x = np.asarray(req.instances, dtype=float)
        probs = bundle["model"].predict_proba(x)
    except Exception as exc:  # noqa: BLE001 - surface as 500 with a metric
        PREDICTIONS.labels(status="error").inc()
        raise HTTPException(status_code=500, detail="prediction failed") from exc
    LATENCY.observe(time.perf_counter() - start)
    PREDICTIONS.labels(status="ok").inc()
    INSTANCES.inc(len(req.instances))
    classes = bundle["classes"]
    return PredictResponse(
        predictions=[classes[i] for i in probs.argmax(axis=1)],
        probabilities=[[round(float(p), 6) for p in row] for row in probs],
        model_version=state["version"],
    )


@app.get("/metrics")
def metrics() -> Response:
    return Response(generate_latest(), media_type=CONTENT_TYPE_LATEST)
