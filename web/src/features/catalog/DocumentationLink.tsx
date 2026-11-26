import { BookOpen } from "lucide-react"

const docs = "https://github.com/Zhenyu-Sun-86587/platform-smolvm/tree/deep-runtime/main/docs/deep-runtime"

export function DocumentationLink({ className }: { className?: string }) {
  return <a className={className} data-slot="console-button" data-variant="secondary" data-size="md" href={docs}>
    <BookOpen size={14} /> Documentation
  </a>
}
