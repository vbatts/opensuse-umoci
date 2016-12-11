% umoci-remove(1) # umoci tag - Remove tags from OCI images
% Aleksa Sarai
% DECEMBER 2016
# NAME
umoci remove - Removes tags from OCI images

# SYNOPSIS
**umoci remove**
**--image**=*image*[:*tag*]

**umoci rm**
**--image**=*image*[:*tag*]

# DESCRIPTION
Removes the given tag from the OCI image.

# OPTIONS

**--image**=*image*[:*tag*]
  The source OCI image tag to remove. *image* must be a path to a valid OCI
  image and *tag* must be a valid tag name (**umoci-remove**(1) does not return
  an error if the tag did not exist). If *tag* is not provided it defaults to
  "latest".

# SEE ALSO
**umoci**(1), **umoci-tag**(1)