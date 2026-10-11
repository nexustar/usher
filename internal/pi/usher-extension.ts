import { stat } from "node:fs/promises";
import { basename, extname, resolve } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";

const params = Type.Object({
	file_path: Type.String({ description: "Image path, absolute or relative to the working directory." }),
});

const supported = new Set([".png", ".jpg", ".jpeg", ".gif", ".webp"]);

export default function usherExtension(pi: ExtensionAPI) {
	pi.registerTool({
		name: "show_image",
		label: "Show image",
		description:
			"Show an image to the user. Call it whenever you have produced or found an image the user should see " +
			"(a chart, screenshot, render): saving a file or printing its path does not display it. " +
			"Supports .png, .jpg, .jpeg, .gif and .webp.",
		parameters: params,
		async execute(_toolCallId, input, _signal, _onUpdate, ctx) {
			const raw = input.file_path.trim();
			if (!raw) throw new Error("show_image: file_path is required");
			const path = resolve(ctx.cwd, raw);
			if (!supported.has(extname(path).toLowerCase())) {
				throw new Error("show_image: supported types are .png, .jpg, .jpeg, .gif, and .webp");
			}
			const info = await stat(path);
			if (!info.isFile()) throw new Error(`show_image: not a regular file: ${raw}`);
			return {
				content: [{ type: "text", text: JSON.stringify({ message: `Showing ${basename(path)} to the user.` }) }],
				details: { file_path: raw },
			};
		},
	});
}
