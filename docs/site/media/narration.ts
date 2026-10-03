// The one narration voice every docs video uses, and the call that speaks a section's text in it.
// A section's own video imports `NARRATION_VOICE` (or runs through `walkthrough.ts`, which does)
// rather than restating the values.
import { writeFileSync } from "node:fs";

/** ElevenLabs voice Daniel, at the settings every docs video is narrated with. */
export const NARRATION_VOICE = {
  name: "Daniel",
  voiceId: "onwK4e9ZLuTAKqWW03F9",
  model: "eleven_turbo_v2_5",
  outputFormat: "mp3_44100_192",
  settings: {
    stability: 0.75,
    similarity_boost: 0.85,
    style: 0,
    speed: 0.92,
    use_speaker_boost: true,
  },
} as const;

/**
 * Speaks `text` in `NARRATION_VOICE` and writes the MP3 to `out`. `previous` and `next` are the
 * neighbouring sections' text, which ElevenLabs reads for continuous intonation across cuts. The
 * key comes from `ELEVENLABS_API_KEY`, trimmed (a stored key has carried a trailing newline, which
 * an HTTP header refuses), and no error this raises carries it.
 */
export async function narrate(
  text: string,
  out: string,
  context: { readonly previous: string; readonly next: string }
): Promise<void> {
  const key = process.env.ELEVENLABS_API_KEY?.trim();
  if (!key) throw new Error("ELEVENLABS_API_KEY must be set to narrate.");
  const voice = NARRATION_VOICE;
  let response: Response;
  try {
    response = await fetch(
      `https://api.elevenlabs.io/v1/text-to-speech/${voice.voiceId}?output_format=${voice.outputFormat}`,
      {
        body: JSON.stringify({
          model_id: voice.model,
          next_text: context.next.slice(0, 200),
          previous_text: context.previous.slice(-200),
          text,
          voice_settings: voice.settings,
        }),
        headers: { Accept: "audio/mpeg", "Content-Type": "application/json", "xi-api-key": key },
        method: "POST",
      }
    );
  } catch (error) {
    // The error's name only: a transport error can quote the request, whose headers hold the key.
    throw new Error(
      `Could not reach ElevenLabs (${error instanceof Error ? error.name : "error"})`
    );
  }
  if (!response.ok) {
    const body = (await response.text()).slice(0, 300);
    throw new Error(`ElevenLabs answered ${response.status}: ${body}`);
  }
  writeFileSync(out, new Uint8Array(await response.arrayBuffer()));
}
