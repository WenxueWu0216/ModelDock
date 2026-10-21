"""Train the sample model baked into the model-server image.

A logistic regression on the Iris dataset: small, deterministic and fast to
train at image build time, which is all the deployable example needs.
"""

import argparse
import json

import joblib
from sklearn.datasets import load_iris
from sklearn.linear_model import LogisticRegression
from sklearn.model_selection import train_test_split
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import StandardScaler


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", default="model.joblib")
    args = parser.parse_args()

    iris = load_iris()
    x_train, x_test, y_train, y_test = train_test_split(
        iris.data, iris.target, test_size=0.2, random_state=42, stratify=iris.target
    )
    model = make_pipeline(StandardScaler(), LogisticRegression(max_iter=500, random_state=42))
    model.fit(x_train, y_train)
    accuracy = float(model.score(x_test, y_test))

    joblib.dump(
        {
            "model": model,
            "classes": [str(c) for c in iris.target_names],
            "features": list(iris.feature_names),
            "accuracy": accuracy,
        },
        args.out,
    )
    print(json.dumps({"out": args.out, "test_accuracy": accuracy}))


if __name__ == "__main__":
    main()
