import os
import subprocess
import tempfile

def convert_to_opus_ogg(input_file, output_file=None, bitrate="32k", sample_rate=24000):
    """
    Convert an audio file to Opus format in an Ogg container.
    
    Args:
        input_file (str): Path to the input audio file
        output_file (str, optional): Path to save the output file. If None, replaces the
                                    extension of input_file with .ogg
        bitrate (str, optional): Target bitrate for Opus encoding (default: "32k")
        sample_rate (int, optional): Sample rate for output (default: 24000)
    
    Returns:
        str: Path to the converted file
        
    Raises:
        FileNotFoundError: If the input file doesn't exist
        RuntimeError: If the ffmpeg conversion fails
    """
    if not os.path.isfile(input_file):
        raise FileNotFoundError(f"Input file not found: {input_file}")
    
    # If no output file is specified, replace the extension with .ogg
    if output_file is None:
        output_file = os.path.splitext(input_file)[0] + ".ogg"
    
    # Ensure the output directory exists
    output_dir = os.path.dirname(output_file)
    if output_dir and not os.path.exists(output_dir):
        os.makedirs(output_dir)
    
    # Build the ffmpeg command
    cmd = [
        "ffmpeg",
        "-nostdin",
        "-hide_banner",
        "-loglevel", "error",
        "-i", input_file,
        "-vn",                       # audio only
        "-c:a", "libopus",
        "-b:a", bitrate,
        "-ar", str(sample_rate),
        "-application", "voip",  # Optimize for voice
        "-vbr", "on",           # Variable bitrate
        "-compression_level", "10",  # Maximum compression
        "-frame_duration", "60",     # 60ms frames (good for voice)
        "-y",                        # Overwrite output file if it exists
        output_file
    ]
    
    try:
        # Run the ffmpeg command and capture output
        subprocess.run(
            cmd,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            check=True,
            timeout=300,
        )
        return output_file
    except FileNotFoundError:
        raise RuntimeError("ffmpeg is not installed or not on PATH")
    except subprocess.TimeoutExpired:
        raise RuntimeError("ffmpeg took too long to convert the audio file")
    except subprocess.CalledProcessError as e:
        raise RuntimeError(f"Failed to convert audio: {(e.stderr or '').strip()[-500:]}")


def convert_to_opus_ogg_temp(input_file, directory=None, bitrate="32k", sample_rate=24000):
    """
    Convert an audio file to Opus format in an Ogg container, in a temporary file.

    Args:
        input_file (str): Path to the input audio file
        directory (str, optional): Folder for the temporary file (created private to
                                   the current user if missing). Defaults to the system
                                   temporary folder.
        bitrate (str, optional): Target bitrate for Opus encoding (default: "32k")
        sample_rate (int, optional): Sample rate for output (default: 24000)

    Returns:
        str: Path to the temporary file with the converted audio. The caller deletes it.

    Raises:
        FileNotFoundError: If the input file doesn't exist
        RuntimeError: If the ffmpeg conversion fails
    """
    if not os.path.isfile(input_file):
        raise FileNotFoundError(f"Input file not found: {input_file}")
    if directory:
        os.makedirs(directory, mode=0o700, exist_ok=True)

    # mkstemp creates the file with owner-only permissions
    handle, temp_path = tempfile.mkstemp(suffix=".ogg", prefix="voice-", dir=directory)
    os.close(handle)

    try:
        convert_to_opus_ogg(input_file, temp_path, bitrate, sample_rate)
        return temp_path
    except Exception:
        # Clean up the temporary file if conversion fails
        if os.path.exists(temp_path):
            os.unlink(temp_path)
        raise


if __name__ == "__main__":
    # Example usage
    import sys
    
    if len(sys.argv) < 2:
        print("Usage: python audio.py input_file [output_file]")
        sys.exit(1)
    
    input_file = sys.argv[1]
    
    try:
        if len(sys.argv) > 2:
            result = convert_to_opus_ogg(input_file, sys.argv[2])
        else:
            result = convert_to_opus_ogg_temp(input_file)
        print(f"Successfully converted to: {result}")
    except Exception as e:
        print(f"Error: {e}")
        sys.exit(1)
