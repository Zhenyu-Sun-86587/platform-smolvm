export async function fileDigest(source: Blob | ReadableStream<Uint8Array>) {
  const { sha256 } = await import("@noble/hashes/sha256")
  const hash = sha256.create()
  const reader = (source instanceof Blob ? source.stream() : source).getReader()
  try {
    for (;;) {
      const chunk = await reader.read()
      if (chunk.done) break
      hash.update(chunk.value)
    }
  } finally {
    reader.releaseLock()
  }
  return Array.from(hash.digest(), byte => byte.toString(16).padStart(2, "0")).join("")
}
