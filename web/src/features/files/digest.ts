export async function fileDigest(blob: Blob) {
  const { sha256 } = await import("@noble/hashes/sha256")
  const hash = sha256.create()
  const reader = blob.stream().getReader()
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
