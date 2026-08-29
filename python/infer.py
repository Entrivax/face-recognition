#!/usr/bin/env python3
"""Face inference sidecar for the `recogn` Go application.

Speaks NDJSON over stdin/stdout. One JSON object per line, both directions.

Requests from Go:
  {"id": <int>, "cmd": "detect", "image": "<base64 image bytes>"}
  {"id": <int>, "cmd": "embed",  "image": "<base64 aligned 112x112 image>"}
  {"id": <int>, "cmd": "ping"}

Responses to Go (one per request, "id" echoed):
  detect -> {"id":.., "faces":[{"bbox":[x,y,w,h], "score":f, "landmarks":[[x,y]...5]}]}
  embed  -> {"id":.., "embedding":[512 floats]}
  ping   -> {"id":.., "status":"ok"}
  error  -> {"id":.., "error":"message"}

All diagnostics go to stderr; stdout carries protocol frames only.
"""
import sys
import os
import json
import base64
import io

import numpy as np
import cv2
import onnxruntime as ort

DET_SIZE = 640  # SCRFD network input size (square)
PROVIDERS = ["CPUExecutionProvider"]


def log(*a):
    print(*a, file=sys.stderr, flush=True)


# --------------------------------------------------------------------------
# SCRFD face detector
# --------------------------------------------------------------------------
class SCRFD:
    def __init__(self, model_path):
        self.session = ort.InferenceSession(model_path, providers=PROVIDERS)
        self.input_name = self.session.get_inputs()[0].name
        self.center_cache = {}
        self.nms_thresh = 0.4
        self.det_thresh = 0.5

    def _distance2bbox(self, points, distance, stride):
        x1 = points[:, 0] - distance[:, 0]
        y1 = points[:, 1] - distance[:, 1]
        x2 = points[:, 0] + distance[:, 2]
        y2 = points[:, 1] + distance[:, 3]
        return np.stack([x1, y1, x2, y2], axis=-1)

    def _distance2kps(self, points, distance, stride):
        preds = []
        for i in range(0, distance.shape[1], 2):
            px = points[:, i % 2] + distance[:, i]
            py = points[:, i % 2 + 1] + distance[:, i + 1]
            preds.append(px)
            preds.append(py)
        return np.stack(preds, axis=-1)

    def detect(self, img, input_size=DET_SIZE, thresh=None, max_num=0):
        thresh = thresh if thresh is not None else self.det_thresh
        im_ratio = float(img.shape[0]) / img.shape[1]
        model_ratio = float(input_size) / input_size
        if im_ratio > model_ratio:
            new_height = input_size
            new_width = int(new_height / im_ratio)
        else:
            new_width = input_size
            new_height = int(new_width * im_ratio)
        det_scale = float(new_height) / img.shape[0]
        resized = cv2.resize(img, (new_width, new_height))
        det_img = np.zeros((input_size, input_size, 3), dtype=np.uint8)
        det_img[:new_height, :new_width, :] = resized

        blob = cv2.dnn.blobFromImage(
            det_img, 1.0 / 128.0, (input_size, input_size),
            (127.5, 127.5, 127.5), swapRB=True)
        outputs = self.session.run(None, {self.input_name: blob})

        input_height = blob.shape[2]
        input_width = blob.shape[3]
        fmc = 3
        feat_stride_fpn = [8, 16, 32]
        scores_list, bboxes_list, kpss_list = [], [], []
        num_anchors = 2
        for idx, stride in enumerate(feat_stride_fpn):
            scores = outputs[idx]
            bbox_preds = outputs[idx + fmc] * stride
            kps_preds = outputs[idx + fmc * 2] * stride
            height = input_height // stride
            width = input_width // stride
            key = (height, width, stride)
            if key in self.center_cache:
                anchor_centers = self.center_cache[key]
            else:
                anchor_centers = np.stack(
                    np.mgrid[:height, :width][::-1], axis=-1).astype(np.float32)
                anchor_centers = (anchor_centers * stride).reshape((-1, 2))
                if num_anchors > 1:
                    anchor_centers = np.stack(
                        [anchor_centers] * num_anchors, axis=1).reshape((-1, 2))
                self.center_cache[key] = anchor_centers

            pos_inds = np.where(scores >= thresh)[0]
            bboxes = self._distance2bbox(anchor_centers, bbox_preds, stride)
            pos_scores = scores[pos_inds]
            pos_bboxes = bboxes[pos_inds]
            scores_list.append(pos_scores)
            bboxes_list.append(pos_bboxes)
            kpss = self._distance2kps(anchor_centers, kps_preds, stride)
            kpss = kpss.reshape((kpss.shape[0], -1, 2))
            pos_kpss = kpss[pos_inds]
            kpss_list.append(pos_kpss)

        scores = np.vstack(scores_list).ravel()
        bboxes = np.vstack(bboxes_list) / det_scale
        kpss = np.vstack(kpss_list) / det_scale
        pre_det = np.hstack((bboxes, scores[:, np.newaxis])).astype(np.float32)
        keep = self._nms(pre_det)
        det = pre_det[keep, :]
        kpss = kpss[keep, :, :]
        if max_num > 0 and det.shape[0] > max_num:
            area = (det[:, 2] - det[:, 0]) * (det[:, 3] - det[:, 1])
            order = np.argsort(area)[::-1][:max_num]
            det = det[order]
            kpss = kpss[order]
        return det, kpss

    def _nms(self, dets):
        x1, y1, x2, y2, scores = (dets[:, 0], dets[:, 1], dets[:, 2],
                                  dets[:, 3], dets[:, 4])
        areas = (x2 - x1 + 1) * (y2 - y1 + 1)
        order = scores.argsort()[::-1]
        keep = []
        while order.size > 0:
            i = order[0]
            keep.append(i)
            xx1 = np.maximum(x1[i], x1[order[1:]])
            yy1 = np.maximum(y1[i], y1[order[1:]])
            xx2 = np.minimum(x2[i], x2[order[1:]])
            yy2 = np.minimum(y2[i], y2[order[1:]])
            w = np.maximum(0.0, xx2 - xx1 + 1)
            h = np.maximum(0.0, yy2 - yy1 + 1)
            inter = w * h
            ovr = inter / (areas[i] + areas[order[1:]] - inter)
            inds = np.where(ovr <= self.nms_thresh)[0]
            order = order[inds + 1]
        return keep


# --------------------------------------------------------------------------
# ArcFace embedder
# --------------------------------------------------------------------------
class ArcFace:
    def __init__(self, model_path):
        self.session = ort.InferenceSession(model_path, providers=PROVIDERS)
        self.input_name = self.session.get_inputs()[0].name

    def embed(self, img112):
        blob = cv2.dnn.blobFromImage(
            img112, 1.0 / 127.5, (112, 112), (127.5, 127.5, 127.5),
            swapRB=True)
        out = self.session.run(None, {self.input_name: blob})[0]
        emb = out[0].astype(np.float32)
        n = np.linalg.norm(emb)
        if n > 0:
            emb = emb / n
        return emb


# --------------------------------------------------------------------------
# Sidecar protocol loop
# --------------------------------------------------------------------------
_DETECTOR = None
_EMBEDDER = None


def _decode_image(b64):
    data = base64.b64decode(b64)
    arr = np.frombuffer(data, dtype=np.uint8)
    img = cv2.imdecode(arr, cv2.IMREAD_COLOR)
    if img is None:
        raise ValueError("could not decode image")
    return img


def handle(req):
    cmd = req.get("cmd")
    if cmd == "ping":
        return {"status": "ok"}
    if cmd == "detect":
        img = _decode_image(req["image"])
        det, kpss = _DETECTOR.detect(img)
        faces = []
        for i in range(det.shape[0]):
            x1, y1, x2, y2, score = det[i]
            lm = kpss[i].tolist() if kpss is not None else []
            faces.append({
                "bbox": [float(x1), float(y1),
                         float(x2 - x1), float(y2 - y1)],
                "score": float(score),
                "landmarks": [[float(p[0]), float(p[1])] for p in lm],
            })
        return {"faces": faces}
    if cmd == "embed":
        img = _decode_image(req["image"])
        emb = _EMBEDDER.embed(img)
        return {"embedding": emb.tolist()}
    raise ValueError("unknown cmd: %r" % (cmd,))


def main():
    global _DETECTOR, _EMBEDDER
    det_path = os.environ.get("RECOGN_DET_MODEL", "models/det_10g.onnx")
    emb_path = os.environ.get("RECOGN_EMB_MODEL", "models/w600k_r50.onnx")
    _DETECTOR = SCRFD(det_path)
    _EMBEDDER = ArcFace(emb_path)
    log("sidecar ready (det=%s emb=%s)" % (det_path, emb_path))

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except Exception as e:
            resp = {"id": None, "error": "bad json: %s" % e}
            print(json.dumps(resp), flush=True)
            continue
        rid = req.get("id")
        try:
            out = handle(req)
            out["id"] = rid
        except Exception as e:
            out = {"id": rid, "error": str(e)}
        print(json.dumps(out), flush=True)


if __name__ == "__main__":
    main()
