import { useRef, useState } from 'react'
import { inRestaurant, post } from '../lib/api'
import type { Image, ImageUpload as Upload } from '../lib/types'
import { ErrorBox } from './ui'

// The three-step upload the API asks for:
//   1. reserve: POST /images with the type and size → a presigned URL
//   2. PUT the bytes straight to object storage
//   3. complete: the server decodes the file, rejects anything that is not
//      really an image, and generates the thumb/card/full sizes
// Only after step 3 is the image attachable to an item.
export default function ImageUpload({
  restaurantID,
  onUploaded,
}: {
  restaurantID: string
  onUploaded: (image: Image) => void
}) {
  const input = useRef<HTMLInputElement>(null)
  const [step, setStep] = useState<string | null>(null)
  const [error, setError] = useState<unknown>(null)

  async function upload(file: File) {
    setError(null)
    try {
      setStep('Reserving…')
      const reserved = await post<Upload>(inRestaurant(restaurantID, '/images'), {
        content_type: file.type,
        byte_size: file.size,
      })

      setStep('Uploading…')
      const put = await fetch(reserved.upload_url, {
        method: 'PUT',
        headers: { 'Content-Type': reserved.upload_with_content_type },
        body: file,
      })
      if (!put.ok) throw new Error(`Storage refused the upload (${put.status}).`)

      setStep('Processing…')
      onUploaded(await post<Image>(reserved.complete_url))
    } catch (err) {
      setError(err)
    } finally {
      setStep(null)
      if (input.current) input.current.value = ''
    }
  }

  return (
    <div>
      <input
        ref={input}
        type="file"
        accept="image/jpeg,image/png,image/webp"
        disabled={step !== null}
        onChange={(e) => e.target.files?.[0] && upload(e.target.files[0])}
      />
      {step && <span className="muted"> {step}</span>}
      <ErrorBox error={error} />
    </div>
  )
}
