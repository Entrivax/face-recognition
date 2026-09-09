// Face-check viewer.
// Enlarged preview used by the enroll modal: shows a pending photo with its
// detected faces drawn over it before anything is uploaded. Read-only; it
// stacks on top of the other modals. Faces may arrive after the viewer opens
// (the enroll modal's check chain reports them via App).

import { useEffect, useRef } from "preact/hooks";
import type { CheckView, Face } from "../types";
import { drawWhenReady } from "../overlay";
import { Modal } from "./Modal";

interface FaceCheckModalProps {
	view: CheckView | null;
	onCloseRequest: () => void;
}

export function FaceCheckModal({ view, onCloseRequest }: FaceCheckModalProps) {
	const imgRef = useRef<HTMLImageElement | null>(null);
	const canvasRef = useRef<HTMLCanvasElement | null>(null);

	// Draw the boxes whenever the viewed photo's faces change (they may
	// arrive after the image).
	useEffect(() => {
		if (!view || !view.faces || !imgRef.current || !canvasRef.current) return;
		drawWhenReady(canvasRef.current, imgRef.current, view.faces, { labels: true });
	}, [view]);

	function verdictText(faces: Face[] | null): { text: string; warn: boolean } {
		if (!faces) return { text: "Detecting faces…", warn: false };
		if (!faces.length) {
			return { text: "No face detected — this photo will be rejected at enrollment.", warn: true };
		}
		const parts = faces.map((f) => {
			const conf = Math.round((f.confidence || 0) * 100);
			return f.name && f.name !== "unknown"
				? `${f.name} ${conf}%`
				: `unknown (under threshold)`;
		});
		const n = faces.length;
		const lead = n === 1 ? "1 face detected" : `${n} faces detected — the largest face is used`;
		return { text: `${lead} · ${parts.join(", ")}`, warn: false };
	}

	const v = verdictText(view?.faces ?? null);

	return (
		<Modal
			open={view !== null}
			id="checkModal"
			cardId="checkCard"
			eyebrow="face check"
			titleId="checkTitle"
			title={view?.title || "Face check"}
			onCloseRequest={onCloseRequest}
			body={
				<div class="modal-body">
					<div class="canvas-wrap photo-canvas">
						<img ref={imgRef} id="checkImg" alt="Photo under review" src={view?.src || undefined} />
						<canvas ref={canvasRef} id="checkOverlay" />
					</div>
					<p class={"photo-verdict" + (v.warn ? " warn" : "")} id="checkVerdict" aria-live="polite">{v.text}</p>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close>Back</button>
					</footer>
				</div>
			}
		/>
	);
}
